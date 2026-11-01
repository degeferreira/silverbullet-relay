"""Regenerate this repository's native SilverBullet library manifest."""
import hashlib
import json
from pathlib import Path
name='Relay'
root=Path(__file__).resolve().parent;library=root/'Library'
files=sorted(str(path.relative_to(library)) for path in library.rglob('*') if path.is_file() and path.name!='Manifest.md')
lines=['---','tags: meta/library','name: Library/'+name+'/Manifest','version: '+(root/'VERSION').read_text().strip(),'files:']
lines.extend('  - '+json.dumps(path,ensure_ascii=False) for path in files)
lines.append('asset_hashes:')
lines.extend('  '+json.dumps(path,ensure_ascii=False)+': '+hashlib.sha256((library/path).read_bytes()).hexdigest() for path in files)
lines.extend(['---','','# '+name,'','Biblioteca para SilverBullet 2.11.1. Requer o serviço auxiliar da mesma versão.','Instalar esta biblioteca não instala nem configura o serviço no servidor.',''])
(library/'Manifest.md').write_text('\n'.join(lines))
