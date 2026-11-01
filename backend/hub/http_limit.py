"""Bound chunked request bodies before JSON parsing allocates the full payload."""
from starlette.exceptions import HTTPException
class BodyLimit:
    def __init__(self,app):self.app=app
    async def __call__(self,scope,receive,send):
        if scope['type']!='http':return await self.app(scope,receive,send)
        limit=24_100_000 if scope.get('path')=='/v1/paperwork/documents' else 2_100_000 if scope.get('path','').endswith('/propose') else 65536
        received=0
        async def bounded_receive():
            nonlocal received
            message=await receive()
            if message['type']=='http.request':
                received+=len(message.get('body',b''))
                if received>limit:raise HTTPException(413,'Request body too large')
            return message
        return await self.app(scope,bounded_receive,send)
