"""Initialize only a clean package. Existing installations use migration tooling."""
import argparse
import json
import os
import shutil
from pathlib import Path

def initialize(root):
    root=Path(root).resolve()
    if not (root/'package.json').is_file():raise ValueError('Run package.py first')
    runtime=root/'runtime'
    if runtime.exists():raise ValueError('Runtime already exists; refusing to reset configuration')
    runtime.mkdir(mode=0o700)
    for name in ('silverbullet','space','config','state','secrets','calendar','paperwork','backups'):
        (runtime/name).mkdir(mode=0o700)
    (runtime/'state/configuration/secrets').mkdir(parents=True,mode=0o700)
    metadata=json.loads((root/'package.json').read_text())
    target=runtime/'paperwork/config.json' if metadata.get('kind')=='paperwork' else runtime/'config/integrations.json'
    shutil.copy2(root/'config.example.json',target)
    env=root/'.env'
    if not env.exists():
        fd=os.open(env,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
        with os.fdopen(fd,'w') as output:output.write('PUID='+str(getattr(os,"getuid",lambda:1000)())+'\nPGID='+str(getattr(os,"getgid",lambda:1000)())+'\n')
    if metadata.get('kind')=='paperwork':
        (runtime/'space/index.md').write_text('# Paperwork\n\n${paperwork.home()}\n\nConfigure o acesso ao Space antes de incorporar notas.\n')
    for name in ('silverbullet_token','vikunja_token','zotero_key','readeck_token'):
        fd=os.open(runtime/'secrets'/name,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600);os.close(fd)
    return runtime

if __name__=='__main__':
    cli=argparse.ArgumentParser(description=__doc__);cli.add_argument('package')
    print(initialize(cli.parse_args().package))
