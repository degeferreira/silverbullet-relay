"""Run with the backend stopped. Does not print secrets or contact services."""
import getpass
import os
from pathlib import Path
import sys
def private_write(path,value):
    import tempfile
    fd,name=tempfile.mkstemp(prefix='.credential-',dir=path.parent)
    try:
        with os.fdopen(fd,'w') as output:
            output.write(value);output.flush();os.fsync(output.fileno())
        os.replace(name,path)
    finally:
        Path(name).unlink(missing_ok=True)
root=Path(sys.argv[1] if len(sys.argv)>1 else 'runtime/secrets')
root.mkdir(parents=True,exist_ok=True); root.chmod(0o700)
for name in ('silverbullet_token','vikunja_token','zotero_key','readeck_token'):
    value=getpass.getpass(name+' (empty to keep): ')
    if value:
        private_write(root/name,value.strip()+'\n')
print('Credentials saved. Start the backend when configuration is ready.')
