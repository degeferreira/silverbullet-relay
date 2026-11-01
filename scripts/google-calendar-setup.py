#!/usr/bin/env python3
"""Authorize over localhost/SSH forwarding; no public callback or new packages."""
import argparse
import base64
import hashlib
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
from pathlib import Path
import secrets
import time
from urllib.parse import parse_qs, urlencode, urlparse
from urllib.request import Request, urlopen
import uuid

ROOT = Path(__file__).resolve().parents[1]
FOLDER = Path(os.environ.get('RELAY_CALENDAR_DIRECTORY', str(ROOT / 'runtime/calendar')))
SCOPE = 'https://www.googleapis.com/auth/calendar.app.created'

def save(path, value):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd = os.open(path.with_suffix('.tmp'), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, 'w') as stream:
        json.dump(value, stream, indent=2)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(path.with_suffix('.tmp'), path)

def request(url, data, token=None):
    headers = {'Content-Type': 'application/json' if token else 'application/x-www-form-urlencoded'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    payload = json.dumps(data).encode() if token else urlencode(data).encode()
    with urlopen(Request(url, payload, headers), timeout=30) as response:
        return json.load(response)

def main():
    if os.geteuid() == 0:
        raise SystemExit('Execute como o usuário dono da instalação, sem sudo, para preservar as permissões.')
    parser = argparse.ArgumentParser()
    parser.add_argument('--client', type=Path)
    parser.add_argument('--create-calendar', action='store_true')
    parser.add_argument('--calendar-id', help='Recover an already-created calendar after ambiguous creation')
    parser.add_argument('--enable', action='store_true')
    args = parser.parse_args()
    FOLDER.mkdir(parents=True, exist_ok=True, mode=0o700)
    os.chmod(FOLDER, 0o700)
    if args.client:
        client = json.loads(args.client.read_text()).get('installed')
        if not client:
            raise ValueError('Use uma credencial OAuth Desktop, não Web ou service account')
        state, verifier = secrets.token_urlsafe(32), secrets.token_urlsafe(48)
        redirect = 'http://127.0.0.1:8765/'
        result = {}
        class Callback(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass
            def do_GET(self):
                query = parse_qs(urlparse(self.path).query)
                valid = secrets.compare_digest(query.get('state', [''])[0], state)
                if valid and query.get('code'):
                    result['code'] = query['code'][0]
                self.send_response(200 if result else 400)
                self.end_headers()
                self.wfile.write(b'Autorizacao recebida. Pode fechar esta janela.' if result else b'Autorizacao invalida.')
        server = HTTPServer(('127.0.0.1', 8765), Callback)
        server.timeout = 10
        challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip('=')
        url = 'https://accounts.google.com/o/oauth2/v2/auth?' + urlencode({
            'client_id': client['client_id'], 'redirect_uri': redirect, 'response_type': 'code',
            'scope': SCOPE, 'access_type': 'offline', 'prompt': 'consent', 'state': state,
            'code_challenge': challenge, 'code_challenge_method': 'S256'})
        print('Com SSH -L 8765:127.0.0.1:8765 conectado, abra no navegador do computador:\n' + url, flush=True)
        deadline = time.monotonic() + 600
        try:
            while not result and time.monotonic() < deadline:
                server.handle_request()
        finally:
            server.server_close()
        if not result:
            raise TimeoutError('Autorização não recebida em dez minutos')
        tokens = request('https://oauth2.googleapis.com/token', {
            'grant_type': 'authorization_code', 'code': result['code'], 'redirect_uri': redirect,
            'client_id': client['client_id'], 'client_secret': client['client_secret'], 'code_verifier': verifier})
        if not tokens.get('refresh_token'):
            raise ValueError('Google não retornou refresh_token; refaça consentimento offline')
        save(FOLDER / 'oauth.json', {key: (client if key.startswith('client_') else tokens)[key]
                                   for key in ('client_id', 'client_secret', 'refresh_token')})
        print('OAuth salvo; nenhum token exibido.')
    if args.create_calendar or args.calendar_id:
        path = FOLDER / 'calendar.json'
        connection = json.loads(path.read_text()) if path.exists() else {'instance_id': str(uuid.uuid4())}
        credentials = json.loads((FOLDER / 'oauth.json').read_text())
        tokens = request('https://oauth2.googleapis.com/token', {**credentials, 'grant_type': 'refresh_token'})
        if args.calendar_id:
            if args.calendar_id == 'primary':
                raise ValueError('Use apenas calendário secundário criado por esta aplicação')
            from urllib.parse import quote
            req = Request('https://www.googleapis.com/calendar/v3/calendars/' + quote(args.calendar_id, safe=''),
                          headers={'Authorization': 'Bearer ' + tokens['access_token']})
            with urlopen(req, timeout=30) as response:
                calendar = json.load(response)
            connection['calendar_id'] = calendar['id']
        elif not connection.get('calendar_id'):
            if connection.get('creating'):
                raise ValueError('Criação anterior incerta: recupere o ID na interface Google; não criar duplicata')
            connection['creating'] = True
            save(path, connection)
            calendar = request('https://www.googleapis.com/calendar/v3/calendars',
                {'summary': 'Vikunja', 'description': 'Espelho Vikunja · ' + connection['instance_id'],
                 'timeZone': 'America/Sao_Paulo'}, tokens['access_token'])
            connection['calendar_id'] = calendar['id']
        connection.pop('creating', None)
        save(path, connection)
        print('Calendário dedicado configurado.')
    if args.enable:
        if not (FOLDER / 'oauth.json').exists() or not json.loads((FOLDER / 'calendar.json').read_text()).get('calendar_id'):
            raise ValueError('Autorize e crie o calendário antes de ativar')
        config_path = ROOT / 'runtime/state/configuration/integrations.json'
        if not config_path.exists():config_path = ROOT / 'runtime/config/integrations.json'
        config = json.loads(config_path.read_text())
        config['google_calendar']['enabled'] = True
        save(config_path, config)
        print('Job habilitado; recrie somente integration-service para carregar configuração.')

if __name__ == '__main__':
    main()
