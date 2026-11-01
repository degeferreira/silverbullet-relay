"""Build an isolated deployment tree, never copying runtime or credentials."""
import argparse
import json
import shutil
from pathlib import Path

RELAY=Path(__file__).resolve().parent
ROOT=RELAY.parent

def build(destination, pressroom=False, paperwork=False, pressroom_root=None):
    destination=Path(destination).resolve()
    if destination.exists(): raise ValueError('Destination must not exist')
    destination.mkdir(parents=True)
    relay=RELAY
    shutil.copytree(relay/'backend/hub',destination/'backend/hub',ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
    shutil.copy2(relay/'backend/requirements.txt',destination/'backend/requirements.txt')
    docker=(relay/'backend/Dockerfile').read_text().replace('COPY tests ./tests\n','')
    shutil.copytree(relay/'Library',destination/'libraries/Relay')
    shutil.copy2(relay/'config.example.json',destination/'config.example.json')
    shutil.copy2(relay/'compose.yaml',destination/'compose.yaml')
    shutil.copy2(relay/'configure.py',destination/'configure.py')
    shutil.copy2(relay/'export-abnt.sh',destination/'export-abnt.sh')
    shutil.copytree(relay/'bibliography',destination/'bibliography')
    shutil.copytree(relay/'scripts',destination/'scripts',ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
    if pressroom:
        compose=(destination/'compose.yaml').read_text()
        compose=compose.replace('      HUB_CONFIG: /config/integrations.json','      HUB_EXTENSION: pressroom.extension\n      HUB_PRESSROOM: "1"\n      HUB_CONFIG: /config/integrations.json')
        compose=compose.replace('      - ./libraries/Relay:/space/Library/Relay:ro','      - ./libraries/Relay:/space/Library/Relay:ro\n      - ./libraries/Pressroom:/space/Library/Pressroom:ro')
        (destination/'compose.yaml').write_text(compose)
        example=json.loads((destination/'config.example.json').read_text())
        example['website']={'enabled':False,'blog_enabled':False}
        (destination/'config.example.json').write_text(json.dumps(example,indent=2)+'\n')
        private=Path(pressroom_root) if pressroom_root else ROOT/'silverbullet-pressroom'
        shutil.copytree(private/'backend/pressroom',destination/'backend/pressroom',ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
        shutil.copytree(private/'Library',destination/'libraries/Pressroom')
        docker=docker.replace('COPY hub ./hub','COPY hub ./hub\nCOPY pressroom ./pressroom')
        for module in ('blog','microposts'):
            (destination/'backend/hub'/f'{module}.py').write_text('import sys\nfrom pressroom import '+module+' as implementation\nsys.modules[__name__] = implementation\n')
    if paperwork:
        compose=(destination/'compose.yaml').read_text()
        compose=compose.replace('      - ./libraries/Relay:/space/Library/Relay:ro','      - ./libraries/Relay:/space/Library/Relay:ro\n      - ./libraries/Paperwork:/space/Library/Paperwork:ro')
        compose+='''
  paperwork:
    build: ./backend
    init: true
    restart: unless-stopped
    user: "${PUID:-1000}:${PGID:-1000}"
    network_mode: service:silverbullet
    depends_on: [silverbullet, integration-service]
    command: [python, -m, uvicorn, "paperwork.server:app", --host, "127.0.0.1", --port, "8089", --no-access-log]
    environment:
      PAPERWORK_HOME: /paperwork
      PAPERWORK_CONFIG: /paperwork/config.json
      PAPERWORK_RELAY_CONFIG: /config/integrations.json
      PAPERWORK_RELAY_SETTINGS: /relay-settings
    volumes:
      - ./runtime/paperwork:/paperwork
      - ./runtime/config/integrations.json:/config/integrations.json:ro
      - ./runtime/state/configuration:/relay-settings:ro
      - ./runtime/secrets/silverbullet_token:/run/secrets/silverbullet_token:ro
      - ./runtime/secrets/vikunja_token:/run/secrets/vikunja_token:ro
      - ./runtime/secrets/zotero_key:/run/secrets/zotero_key:ro
      - ./runtime/backups:/paperwork-backups
    read_only: true
    tmpfs: [/tmp]
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    mem_limit: 256m
    memswap_limit: 256m
    pids_limit: 64
    cpus: "0.75"
'''
        (destination/'compose.yaml').write_text(compose)
        shutil.copytree(ROOT/'paperwork/paperwork',destination/'backend/paperwork',ignore=shutil.ignore_patterns('__pycache__','*.pyc'))
        shutil.copytree(ROOT/'paperwork/Library',destination/'libraries/Paperwork')
        (destination/'libraries/Paperwork/Deployment.md').write_text('```space-lua\n-- priority: 20\nconfig.set("paperwork.port", 8089)\n```\n')
        docker=docker.replace('COPY hub ./hub','COPY hub ./hub\nCOPY paperwork ./paperwork').replace('git openssh-client ca-certificates','git openssh-client ca-certificates poppler-utils util-linux')
    (destination/'backend/Dockerfile').write_text(docker)
    (destination/'package.json').write_text(json.dumps({'version':(RELAY/'VERSION').read_text().strip(),'silverbullet':'2.11.1','pressroom':pressroom},indent=2)+'\n')
    return destination

if __name__=='__main__':
    cli=argparse.ArgumentParser(description=__doc__)
    cli.add_argument('destination'); cli.add_argument('--pressroom',action='store_true'); cli.add_argument('--paperwork',action='store_true')
    args=cli.parse_args(); print(build(args.destination,args.pressroom,args.paperwork))
