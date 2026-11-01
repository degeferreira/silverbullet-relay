import asyncio
import json
import os
from pathlib import Path
from types import SimpleNamespace
import tempfile
import unittest
from unittest.mock import patch
from fastapi import FastAPI
import httpx
from hub.settings import router

class SettingsTests(unittest.IsolatedAsyncioTestCase):
    async def test_save_preserves_existing_config_and_stages_credentials(self):
        config=json.loads((Path(__file__).parents[1]/'config.example.json').read_text())
        config['website']={'enabled':True,'blog_enabled':True}
        with tempfile.TemporaryDirectory() as folder,patch.dict(os.environ,{'HUB_SETTINGS_DIR':folder}):
            app=FastAPI();app.include_router(router)
            app.state.service=SimpleNamespace(config=config,lock=asyncio.Lock())
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=app),base_url='http://local') as client:
                body=(await client.get('/v1/settings')).json();body['timezone']='America/Sao_Paulo'
                saved=await client.put('/v1/settings',json=body)
                self.assertEqual(saved.status_code,200)
                persisted=json.loads((Path(folder)/'integrations.json').read_text())
                self.assertEqual(persisted['website'],config['website'])
                self.assertEqual(config['timezone'],'UTC')
                result=await client.put('/v1/settings/secrets/vikunja_token',json={'value':'test-only-secret'})
                self.assertEqual(result.status_code,200)
                self.assertNotIn('test-only-secret',result.text)
                secret=Path(folder)/'pending-secrets/vikunja_token'
                self.assertEqual(secret.stat().st_mode & 0o777,0o600)
                self.assertFalse((Path(folder)/'secrets/vikunja_token').exists())
                invalid=await client.put('/v1/settings/secrets/website_ssh_key',json={'value':'forbidden'})
                self.assertEqual(invalid.status_code,404)
