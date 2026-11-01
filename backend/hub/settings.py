"""Explicit administrator configuration, secrets never returned by GET."""
import copy
import json
import os
from pathlib import Path
from urllib.parse import urlsplit
from zoneinfo import ZoneInfo
from fastapi import APIRouter, HTTPException, Request
from pydantic import BaseModel, Field, ConfigDict

router = APIRouter()

class Settings(BaseModel):
    model_config = ConfigDict(extra='forbid')
    timezone: str = Field(max_length=80)
    vikunja_url: str = Field(max_length=500)
    vikunja_public_url: str = Field(max_length=500)
    inbox_id: int = Field(gt=0)
    saved_filter_id: int = Field(gt=0)
    readeck_url: str = Field(max_length=500)
    zotero_user_id: int | None = Field(default=None, gt=0)
    daily_interval:int=Field(default=30,ge=15,le=86400)
    zotero_interval:int=Field(default=600,ge=60,le=86400)
    readeck_interval:int=Field(default=300,ge=30,le=86400)
    readeck_trigger_labels:str=Field(default='silverbullet, process',max_length=1000)
    readeck_processed_label:str=Field(default='silverbullet-imported',min_length=1,max_length=100)
    google_calendar_enabled:bool=False
    google_calendar_interval:int=Field(default=300,ge=60,le=86400)
    daily_enabled: bool = False
    zotero_enabled: bool = False
    readeck_enabled: bool = False


def validate_url(value):
    url = urlsplit(value)
    if url.scheme not in ('http', 'https') or not url.hostname or url.username or url.password or url.query or url.fragment:
        raise ValueError('Use an HTTP(S) server URL without credentials, query or fragment')
    return value.rstrip('/')


def view(config):
    enabled = config.get('enabled', {})
    return dict(daily_interval=config.get('daily_interval',30),zotero_interval=config.get('zotero_interval',600),
                readeck_interval=config.get('readeck_interval',300),
                readeck_trigger_labels=', '.join(config['readeck'].get('trigger_labels',[])),
                readeck_processed_label=config['readeck'].get('processed_label','silverbullet-imported'),
                google_calendar_enabled=config.get('google_calendar',{}).get('enabled',False),
                google_calendar_interval=config.get('google_calendar',{}).get('interval',300),timezone=config['timezone'], vikunja_url=config['vikunja']['url'],
                vikunja_public_url=config['vikunja']['public_url'], inbox_id=config['vikunja']['inbox_id'],
                saved_filter_id=config['vikunja']['saved_filter_id'], readeck_url=config['readeck']['url'],
                zotero_user_id=config['zotero'].get('user_id'),
                **{k+'_enabled': enabled.get(k, True) for k in ('daily','zotero','readeck')})


def private_write(path, data):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    import tempfile
    fd,name=tempfile.mkstemp(prefix='.'+path.name+'-',dir=path.parent)
    tmp=Path(name)
    try:
        with os.fdopen(fd, 'w') as out:
            out.write(data); out.flush(); os.fsync(out.fileno())
        os.replace(tmp, path)
    finally:
        tmp.unlink(missing_ok=True)


@router.get('/v1/settings')
async def get_settings(request: Request):
    service=request.app.state.service
    result=view(service.config)
    if not result['zotero_user_id'] and getattr(service,'state',None):result['zotero_user_id']=service.state.get('zotero_user')
    return result


@router.put('/v1/settings')
async def put_settings(body: Settings, request: Request):
    service = request.app.state.service
    try:
        zone = ZoneInfo(body.timezone)
        vk, public, rd = map(validate_url, (body.vikunja_url,body.vikunja_public_url,body.readeck_url))
    except (ValueError, KeyError):
        raise HTTPException(422, 'Invalid timezone or server URL') from None
    # Rebinding a running worker can cross-link existing remote identities.
    # Persist the complete configuration and require a controlled restart.
    async with service.lock:
        if vk!=service.config['vikunja']['url'].rstrip('/') and service.state.db.execute('SELECT 1 FROM tasks LIMIT 1').fetchone():
            raise HTTPException(409,'Use a new connection profile/state for another Vikunja instance')
        if body.zotero_user_id!=(service.config['zotero'].get('user_id') or (service.state.get('zotero_user') if getattr(service,'state',None) else None)) and service.state.db.execute("SELECT 1 FROM notes WHERE source='ZOTERO' LIMIT 1").fetchone():
            raise HTTPException(409,'Keep the Zotero identity associated with existing notes')
        if rd!=service.config['readeck']['url'].rstrip('/') and service.state.db.execute("SELECT 1 FROM notes WHERE source='READECK' LIMIT 1").fetchone():
            raise HTTPException(409,'Use a new profile/state for another Readeck instance')
        override=Path(os.environ.get("HUB_SETTINGS_DIR","/state/configuration"))/"integrations.json"
        config=json.loads(override.read_text()) if override.exists() else copy.deepcopy(service.config)
        config['timezone'] = body.timezone
        for name in ('daily','zotero','readeck'):config[name+'_interval']=getattr(body,name+'_interval')
        config['readeck']['trigger_labels']=[label.strip() for label in body.readeck_trigger_labels.split(',') if label.strip()]
        config['readeck']['processed_label']=body.readeck_processed_label
        config.setdefault('google_calendar',{}).update(enabled=body.google_calendar_enabled,interval=body.google_calendar_interval)
        config['vikunja'].update(url=vk, public_url=public,inbox_id=body.inbox_id,saved_filter_id=body.saved_filter_id)
        config['readeck']['url'] = rd
        config['zotero']['user_id'] = body.zotero_user_id
        config['enabled'] = {k:getattr(body,k+'_enabled') for k in ('daily','zotero','readeck')}
        try:
            private_write(Path(os.environ.get("HUB_SETTINGS_DIR","/state/configuration"))/"integrations.json",json.dumps(config,indent=2)+"\n")
        except OSError:
            raise HTTPException(503,'Configuration directory must be writable') from None
    return {'saved':True,'restart_required':True}


class Credential(BaseModel):
    model_config = ConfigDict(extra='forbid')
    value: str = Field(min_length=1,max_length=4096)


@router.put('/v1/settings/secrets/{name}')
async def put_secret(name: str, body: Credential):
    if name not in ('vikunja_token','zotero_key','readeck_token'):
        raise HTTPException(404)
    value=body.value.strip()
    if not value or "\n" in value or "\r" in value:
        raise HTTPException(422,'Invalid credential')
    directory=Path(os.environ.get('HUB_SETTINGS_DIR','/state/configuration'))/'pending-secrets'
    try:
        directory.mkdir(parents=True,exist_ok=True,mode=0o700)
        private_write(directory/name,value+'\n')
    except OSError:
        raise HTTPException(503,'Settings storage unavailable') from None
    return {'saved':True,'restart_required':True}
