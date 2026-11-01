"""One-way calendar mirror using public APIs only; no Vikunja schema changes."""
import asyncio
import datetime as dt
import hashlib
import json
import os
import time
from pathlib import Path
from urllib.parse import quote

import httpx

from .core import Conflict, NotConfigured

API = 'https://www.googleapis.com/calendar/v3'


def atomic_private(path, value):
    tmp = path.with_suffix('.tmp')
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(value, stream)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(tmp, path)


class GoogleCalendar:
    def __init__(self, service):
        self.s = service
        self.folder = Path(service.config.get('google_calendar', {}).get('directory', '/calendar'))
        self.lock = asyncio.Lock()
        self.access = None
        self.expires = 0

    def connection(self):
        try:
            value = json.loads((self.folder / 'calendar.json').read_text())
        except (FileNotFoundError, json.JSONDecodeError):
            raise NotConfigured('Google Calendar: autorização/configuração pendente') from None
        if not value.get('calendar_id') or not value.get('instance_id') or value['calendar_id'] == 'primary':
            raise NotConfigured('Google Calendar: calendário dedicado não configurado')
        return value

    async def headers(self):
        if not self.access or self.expires < time.time() + 60:
            try:
                credentials = json.loads((self.folder / 'oauth.json').read_text())
                data = {key: credentials[key] for key in ('client_id', 'client_secret', 'refresh_token')}
            except (FileNotFoundError, KeyError, json.JSONDecodeError):
                raise NotConfigured('Google Calendar: autorização OAuth pendente') from None
            try:
                response = await self.s.request('POST', 'https://oauth2.googleapis.com/token',
                    data={**data, 'grant_type': 'refresh_token'})
            except httpx.HTTPStatusError as exc:
                if exc.response.status_code in (400, 401):
                    raise NotConfigured('Google Calendar: reautorize a conta Google') from None
                raise
            value = response.json()
            self.access = value['access_token']
            self.expires = time.time() + value.get('expires_in', 3600)
            if value.get('refresh_token'):
                credentials['refresh_token'] = value['refresh_token']
                atomic_private(self.folder / 'oauth.json', credentials)
        return {'Authorization': 'Bearer ' + self.access}

    async def google(self, method, path, **kwargs):
        try:
            return await self.s.request(method, API + path, headers=await self.headers(), **kwargs)
        except httpx.HTTPStatusError as exc:
            if exc.response.status_code != 401:
                raise
            self.access = None
            return await self.s.request(method, API + path, headers=await self.headers(), **kwargs)

    def payload(self, task, instance, projects):
        # Fail closed on changed API fields instead of silently deleting events.
        if (type(task.get('id')) is not int or task['id'] <= 0 or
                not isinstance(task.get('title'), str) or type(task.get('done')) is not bool or
                'due_date' not in task):
            raise ValueError('Unsupported Vikunja task response')
        due = task['due_date']
        if not due or (isinstance(due, str) and due.startswith('0001-01-01')):
            return None
        if not isinstance(due, str):
            raise ValueError('Unsupported Vikunja due date')
        date = dt.datetime.fromisoformat(due.replace('Z', '+00:00'))
        if date.tzinfo is None:
            date = date.replace(tzinfo=self.s.zone)
        day = date.astimezone(self.s.zone).date()
        generation = self.s.state.get('calendar:generation:' + str(task['id']), 0)
        identity = hashlib.sha256(f'{instance}:{task["id"]}:{generation}'.encode()).hexdigest()
        base = self.s.config['vikunja']['public_url'].rstrip('/')
        description = (f'Vikunja · tarefa #{task["id"]}\n'
                       f'Projeto: {projects.get(task.get("project_id"), "")}\n'
                       f'{base}/tasks/{task["id"]}\n'
                       'Espelho automático. Edite a tarefa no Vikunja.')
        return {'id': identity, 'summary': ('✅ ' if task['done'] else '') + task['title'],
                'description': description, 'start': {'date': day.isoformat()},
                'end': {'date': (day + dt.timedelta(days=1)).isoformat()},
                'status': 'confirmed', 'transparency': 'transparent', 'reminders': {'useDefault': False},
                'extendedProperties': {'private': {'hubInstance': instance, 'vikunjaTaskId': str(task['id'])}}}

    async def tasks(self):
        # Independent defensive pagination: missing headers must not truncate a scan.
        output, seen, page = [], set(), 1
        while page <= 10000:
            response = await self.s.vk('GET', '/tasks', params={'page': page, 'per_page': 100,
                                                               'sort_by': 'id', 'order_by': 'asc'})
            data = response.json()
            if not isinstance(data, list):
                raise ValueError('Unsupported Vikunja task collection')
            for task in data:
                if not isinstance(task, dict) or type(task.get('id')) is not int or task['id'] in seen:
                    raise ValueError('Unstable Vikunja pagination')
                seen.add(task['id'])
                output.append(task)
            if not data:
                return output
            pages = response.headers.get('x-pagination-total-pages')
            if pages and page >= int(pages):
                return output
            page += 1
        raise ValueError('Vikunja pagination safety limit')

    async def sync(self):
        async with self.lock:
            connection = self.connection()
            instance = connection['instance_id']
            base = '/calendars/' + quote(connection['calendar_id'], safe='') + '/events'
            tasks = await self.tasks()
            projects = {p['id']: p['title'] for p in await self.s.paginated_vk('/projects')}
            # Validate the complete snapshot before making any Google writes.
            expected = {}
            for task in tasks:
                payload = self.payload(task, instance, projects)
                if payload:
                    expected[payload['id']] = payload
            existing, token = {}, None
            while True:
                params = {'privateExtendedProperty': 'hubInstance=' + instance, 'maxResults': 2500}
                if token:
                    params['pageToken'] = token
                data = (await self.google('GET', base, params=params)).json()
                for event in data.get('items', []):
                    if event.get('status') != 'cancelled':
                        existing[event['id']] = event
                token = data.get('nextPageToken')
                if not token:
                    break
            changed = removed = 0
            for identity, payload in expected.items():
                event = existing.get(identity)
                if event is None:
                    try:
                        await self.google('POST', base, json=payload)
                    except httpx.HTTPStatusError as exc:
                        if exc.response.status_code != 409:
                            raise
                        try:
                            event = (await self.google('GET', base + '/' + identity)).json()
                        except httpx.HTTPStatusError as read_error:
                            if read_error.response.status_code not in (404, 410):
                                raise
                            self.bump_generation(payload['extendedProperties']['private']['vikunjaTaskId'])
                            raise Conflict('Google Calendar: evento excluído será recriado no próximo ciclo') from None
                        if event.get('status') == 'cancelled':
                            self.bump_generation(payload['extendedProperties']['private']['vikunjaTaskId'])
                            raise Conflict('Google Calendar: evento excluído será recriado no próximo ciclo') from None
                        if event.get('extendedProperties', {}).get('private') != payload['extendedProperties']['private']:
                            raise Conflict('Google Calendar: identidade de evento incompatível') from None
                        await self.google('PATCH', base + '/' + identity, json={k: v for k, v in payload.items() if k != 'id'})
                    changed += 1
                elif any(event.get(key) != value for key, value in payload.items() if key != 'id'):
                    await self.google('PATCH', base + '/' + identity, json={k: v for k, v in payload.items() if k != 'id'})
                    changed += 1
            for identity, event in existing.items():
                if identity in expected:
                    continue
                private = event.get('extendedProperties', {}).get('private', {})
                task_id = private.get('vikunjaTaskId', '')
                if private.get('hubInstance') != instance or not task_id.isdigit():
                    continue
                # Individual verification prevents deletion after permission loss or
                # a partial/racing task listing. 403 and malformed responses abort.
                try:
                    task = (await self.s.vk('GET', '/tasks/' + task_id)).json()
                except httpx.HTTPStatusError as exc:
                    if exc.response.status_code != 404:
                        raise
                    # A 404 is also possible for lost project access. Only remove
                    # when the project still exists and is accessible to this token.
                    old = self.s.state.get('calendar:task:' + task_id, {})
                    if old.get('project_id') not in projects:
                        continue
                else:
                    current = self.payload(task, instance, projects)
                    if current is not None and current['id'] == identity:
                        continue
                try:
                    await self.google('DELETE', base + '/' + quote(identity, safe=''))
                except httpx.HTTPStatusError as exc:
                    if exc.response.status_code not in (404, 410):
                        raise
                if task_id not in {p['extendedProperties']['private']['vikunjaTaskId'] for p in expected.values()}:
                    self.bump_generation(task_id)
                removed += 1
            for task in tasks:
                self.s.state.set('calendar:task:' + str(task['id']), {'project_id': task.get('project_id')})
            self.s.state.set('calendar:summary', {'tasks': len(expected), 'changed': changed, 'removed': removed})

    def bump_generation(self, task_id):
        key = 'calendar:generation:' + str(task_id)
        self.s.state.set(key, self.s.state.get(key, 0) + 1)

    async def run(self):
        try:
            await self.sync()
        except Exception:
            self.s.state.set('calendar:failures', self.s.state.get('calendar:failures', 0) + 1)
            raise
        else:
            self.s.state.set('calendar:failures', 0)
