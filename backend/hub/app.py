import asyncio
import json
import logging
import os
from contextlib import asynccontextmanager

import httpx
from fastapi import FastAPI, HTTPException, Query, Request
from pydantic import BaseModel, Field

from .core import Conflict, NotConfigured, State, Uncertain
from .service import Service


@asynccontextmanager
async def lifespan(app):
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    logging.getLogger("httpx").setLevel(logging.WARNING)
    from pathlib import Path
    from .settings import private_write
    settings_dir=Path(os.environ.get("HUB_SETTINGS_DIR","/state/configuration"))
    pending=settings_dir/'pending-secrets'
    active=settings_dir/'secrets'
    if pending.exists():
        for credential in pending.iterdir():
            if credential.is_file() and credential.name in ('vikunja_token','zotero_key','readeck_token'):
                private_write(active/credential.name,credential.read_text())
                credential.unlink()
    override=settings_dir/'integrations.json'
    with (override if override.exists() else Path(os.environ.get("HUB_CONFIG", "/config/integrations.json"))).open() as source:
        config=json.load(source)
    app.state.service = Service(config, State(os.environ.get("HUB_STATE", "/state/state.db")))
    if os.environ.get("HUB_PAPERWORK") == "1":
        from paperwork.store import recover_processing
        recover_processing()
    private_write(settings_dir/'effective.json',json.dumps({'google_calendar':{'enabled':config.get('google_calendar',{}).get('enabled',False)},'timezone':config['timezone'],'connections':{'timezone':config['timezone'],'silverbullet_url':config['silverbullet_url'],'vikunja':config['vikunja'],'zotero':{'user_id':config['zotero'].get('user_id') or app.state.service.state.get('zotero_user')}}})+'\n')
    task = asyncio.create_task(app.state.service.worker())
    yield
    task.cancel()
    try:
        await task
    except asyncio.CancelledError:
        pass
    paperwork_tasks=list(getattr(app.state,'paperwork_tasks',set()))
    for pending in paperwork_tasks:pending.cancel()
    if paperwork_tasks:await asyncio.gather(*paperwork_tasks,return_exceptions=True)
    await app.state.service.client.aclose()
    app.state.service.state.db.close()


app = FastAPI(title="SilverBullet personal integrations", lifespan=lifespan,
              docs_url=None, redoc_url=None, openapi_url=None)


from .http_limit import BodyLimit
app.add_middleware(BodyLimit)

@app.middleware("http")
async def local_only(request: Request, call_next):
    if request.client and request.client.host not in ("127.0.0.1", "::1"):
        from fastapi.responses import JSONResponse
        return JSONResponse({"detail": "Loopback only"}, status_code=403)
    limit = 24_100_000 if request.url.path == "/v1/paperwork/documents" else 2_100_000 if request.url.path.endswith("/propose") else 65536
    try:
        declared = int(request.headers.get("content-length", "0"))
    except ValueError:
        from fastapi.responses import JSONResponse
        return JSONResponse({"detail": "Invalid Content-Length"}, status_code=400)
    if declared < 0 or declared > limit:
        from fastapi.responses import JSONResponse
        return JSONResponse({"detail": "Request too large"}, status_code=413)
    return await call_next(request)


async def call(request, method, *args, **kwargs):
    try:
        return await getattr(request.app.state.service, method)(*args, **kwargs)
    except NotConfigured as e:
        raise HTTPException(503, str(e)) from None
    except (Conflict, Uncertain) as e:
        raise HTTPException(409, str(e)) from None
    except ValueError as e:
        raise HTTPException(422, str(e)) from None
    except httpx.HTTPStatusError as e:
        raise HTTPException(502, f"Upstream status {e.response.status_code}") from None
    except httpx.HTTPError:
        raise HTTPException(502, "Upstream unavailable") from None


@app.get("/health/live")
def live():
    return {"ok": True}


@app.get("/health/ready")
@app.get("/v1/status")
async def status(request: Request):
    return request.app.state.service.status()


class DailyRequest(BaseModel):
    path: str = Field(max_length=200)


@app.post("/v1/daily/reconcile")
async def daily(body: DailyRequest, request: Request):
    return await call(request, "reconcile_daily", body.path)


@app.post("/v1/daily/open")
async def open_daily(request: Request):
    return await call(request, "open_daily")


@app.get("/v1/tasks/saved-filter")
async def tasks(request: Request, force: bool = False):
    return await call(request, "saved_filter", force)


@app.get("/v1/tasks/today")
async def today(request: Request):
    return await call(request, "tasks_today")


class TaskPatch(BaseModel):
    title: str | None = Field(default=None, min_length=1, max_length=250)
    done: bool | None = None
    due_date: str | None = Field(default=None, max_length=40)
    priority: int | None = Field(default=None, ge=0, le=5)
    project_id: int | None = Field(default=None, gt=0)
    labels: list[int] | None = Field(default=None, max_length=100)


@app.patch("/v1/tasks/{task_id}")
async def edit(task_id: int, body: TaskPatch, request: Request):
    async with request.app.state.service.lock:
        return await call(request, "update_task", task_id, body.model_dump(exclude_none=True))


@app.get("/v1/tasks/{task_id}")
async def detail(task_id: int, request: Request):
    try:
        return (await request.app.state.service.vk("GET", f"/tasks/{task_id}")).json()
    except NotConfigured as e:
        raise HTTPException(503, str(e)) from None
    except httpx.HTTPError:
        raise HTTPException(502, "Vikunja unavailable") from None


@app.get("/v1/projects")
async def projects(request: Request):
    return await call(request, "paginated_vk", "/projects")


@app.get("/v1/labels")
async def labels(request: Request):
    return await call(request, "paginated_vk", "/labels")


@app.get("/v1/zotero/search")
async def zotero(request: Request, q: str = Query(default="", max_length=200)):
    return await call(request, "zotero_search", q)


@app.post("/v1/zotero/items/{key}/import")
async def literature(key: str, request: Request):
    return await call(request, "import_zotero", key)


@app.get("/v1/zotero/items/{key}/citation")
async def citation(key: str, request: Request):
    return await call(request, "citation", key)


@app.post("/v1/zotero/items/{key}/cite")
async def cite_reference(key: str, request: Request):
    return await call(request, "cite_zotero", key)


@app.get("/v1/readeck/search")
async def readeck(request: Request, q: str = Query(default="", max_length=200)):
    return await call(request, "readeck_search", q)


@app.post("/v1/readeck/bookmarks/{uid}/import")
async def import_bookmark(uid: str, request: Request):
    return await call(request, "import_readeck_id", uid)


# Private website extension is absent from the public distribution.
if os.environ.get("HUB_PRESSROOM") == "1":
    from pressroom.routes import router
    app.include_router(router)

from .settings import router as settings_router
app.include_router(settings_router)

if os.environ.get("HUB_PAPERWORK") == "1":
    from paperwork.api import router as paperwork_router
    app.include_router(paperwork_router)

@app.get('/v1/zotero/identity')
async def zotero_identity(request:Request):
    return {'user_id':await call(request,'zotero_user')}
