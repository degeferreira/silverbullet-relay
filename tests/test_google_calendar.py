import copy
import json
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch
from zoneinfo import ZoneInfo

import httpx

from hub.core import State, Conflict
from hub.service import Service


class CalendarTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        folder = Path(self.tmp.name)
        (folder / 'calendar.json').write_text(json.dumps({'calendar_id': 'dedicated', 'instance_id': 'test-instance'}))
        (folder / 'oauth.json').write_text(json.dumps({'client_id': 'client', 'client_secret': 'secret', 'refresh_token': 'refresh'}))
        self.tasks = [{'id': 7, 'title': 'Teste', 'done': False, 'due_date': '2026-10-06T12:00:00Z', 'project_id': 1}]
        self.events, self.deleted = {}, set()
        self.writes, self.pages = 0, []
        self.bad_page = False
        self.denied = False
        self.fail_list = False
        self.hide_task = False
        self.lost_ack = False
        def transport(req):
            path = req.url.path
            if req.url.host == 'oauth2.googleapis.com':
                return httpx.Response(200, json={'access_token': 'access', 'expires_in': 3600})
            if req.url.host == 'vk':
                if path == '/projects':
                    return httpx.Response(200, json=[{'id': 1, 'title': 'Projeto'}])
                if path == '/tasks':
                    page = int(req.url.params['page'])
                    self.pages.append(page)
                    if self.fail_list:
                        return httpx.Response(503)
                    if self.bad_page and page == 2:
                        return httpx.Response(200, json={'unexpected': True})
                    return httpx.Response(200, json=copy.deepcopy(self.tasks) if page == 1 and not self.hide_task else [])
                if path == '/tasks/7':
                    if self.denied:
                        return httpx.Response(403)
                    return httpx.Response(200, json=copy.deepcopy(self.tasks[0])) if self.tasks else httpx.Response(404)
            if path.endswith('/events') and req.method == 'GET':
                return httpx.Response(200, json={'items': list(copy.deepcopy(self.events).values())})
            if path.endswith('/events') and req.method == 'POST':
                value = json.loads(req.content)
                self.writes += 1
                if value['id'] in self.events or value['id'] in self.deleted:
                    return httpx.Response(409)
                self.events[value['id']] = value
                if self.lost_ack:
                    self.lost_ack = False
                    raise httpx.ReadTimeout('lost acknowledgement', request=req)
                return httpx.Response(200, json=value)
            identity = path.rsplit('/', 1)[-1]
            if req.method == 'GET':
                if identity in self.deleted:
                    return httpx.Response(410)
                return httpx.Response(200, json=self.events[identity])
            if req.method == 'PATCH':
                self.writes += 1
                self.events[identity].update(json.loads(req.content))
                return httpx.Response(200, json=self.events[identity])
            if req.method == 'DELETE':
                self.writes += 1
                self.events.pop(identity, None)
                self.deleted.add(identity)
                return httpx.Response(204)
            raise AssertionError(str(req.url))
        self.service = Service({'timezone': 'America/Sao_Paulo', 'vikunja': {'url': 'http://vk', 'public_url': 'https://private:3456'},
                                'google_calendar': {'directory': str(folder)}}, State(':memory:'),
                               httpx.AsyncClient(transport=httpx.MockTransport(transport)))
        self.secret = patch('hub.service.secret', return_value='token')
        self.secret.start()
        self.calendar = self.service.google_calendar

    async def asyncTearDown(self):
        self.secret.stop()
        await self.service.client.aclose()
        self.service.state.db.close()
        self.tmp.cleanup()

    async def test_create_idempotent_and_timezone(self):
        self.tasks[0]['due_date'] = '2026-10-06T01:00:00Z'
        await self.calendar.sync()
        event = next(iter(self.events.values()))
        self.assertEqual(event['start']['date'], '2026-10-05')
        self.assertEqual(event['end']['date'], '2026-10-06')
        self.assertEqual(event['transparency'], 'transparent')
        self.assertRegex(event['id'], '^[0-9a-f]{64}$')
        writes = self.writes
        await self.calendar.sync()
        self.assertEqual(self.writes, writes)
        self.assertIn(2, self.pages)  # No pagination header: read until empty.

    async def test_title_date_completion_and_reopen(self):
        await self.calendar.sync()
        identity = next(iter(self.events))
        self.tasks[0].update(title='Novo título', done=True, due_date='2026-10-08T12:00:00Z')
        await self.calendar.sync()
        self.assertEqual(self.events[identity]['summary'], '✅ Novo título')
        self.assertEqual(self.events[identity]['start']['date'], '2026-10-08')
        self.tasks[0]['done'] = False
        await self.calendar.sync()
        self.assertEqual(self.events[identity]['summary'], 'Novo título')

    async def test_remove_due_and_restore_new_incarnation(self):
        await self.calendar.sync()
        identity = next(iter(self.events))
        self.tasks[0]['due_date'] = '0001-01-01T00:00:00Z'
        await self.calendar.sync()
        self.assertFalse(self.events)
        self.tasks[0]['due_date'] = '2026-10-08T12:00:00Z'
        await self.calendar.sync()
        self.assertEqual(len(self.events), 1)
        self.assertNotIn(identity, self.events)

    async def test_confirmed_deletion(self):
        await self.calendar.sync()
        self.tasks.clear()
        await self.calendar.sync()
        self.assertFalse(self.events)

    async def test_permission_failure_never_deletes(self):
        await self.calendar.sync()
        self.tasks.clear()
        self.denied = True
        with self.assertRaises(httpx.HTTPStatusError):
            await self.calendar.sync()
        self.assertEqual(len(self.events), 1)

    async def test_missing_listing_but_present_task_kept(self):
        await self.calendar.sync()
        self.hide_task = True
        await self.calendar.sync()
        self.assertEqual(len(self.events), 1)

    async def test_api_change_or_partial_scan_never_writes(self):
        await self.calendar.sync()
        before = self.writes
        self.bad_page = True
        with self.assertRaises(ValueError):
            await self.calendar.sync()
        self.assertEqual(self.writes, before)
        self.bad_page = False
        self.tasks[0].pop('due_date')
        with self.assertRaises(ValueError):
            await self.calendar.sync()
        self.assertEqual(self.writes, before)

    async def test_lost_create_ack_does_not_duplicate(self):
        self.lost_ack = True
        with patch('hub.service.asyncio.sleep', return_value=None):
            await self.calendar.sync()
        self.assertEqual(len(self.events), 1)

    async def test_deleted_event_recreated_without_reusing_tombstone(self):
        await self.calendar.sync()
        identity = next(iter(self.events))
        self.events.clear()
        self.deleted.add(identity)
        with self.assertRaises(Conflict):
            await self.calendar.sync()
        await self.calendar.sync()
        self.assertEqual(len(self.events), 1)
        self.assertNotIn(identity, self.events)

    async def test_manual_events_preserved(self):
        self.events['manual'] = {'id': 'manual', 'summary': 'Compromisso'}
        await self.calendar.sync()
        self.assertIn('manual', self.events)

    async def test_rebuild_from_remote_identity(self):
        await self.calendar.sync()
        self.service.state.db.close()
        self.service.state = State(':memory:')
        before = self.writes
        await self.calendar.sync()
        self.assertEqual(self.writes, before)
