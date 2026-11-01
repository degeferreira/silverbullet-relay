import asyncio
import datetime as dt
import json
import logging
import os
import re
import random
from pathlib import Path
from urllib.parse import quote
from zoneinfo import ZoneInfo

import httpx

from .core import (Conflict, DAILY, NotConfigured, State, Uncertain, apa_markdown, citation_key, digest,
                   html_text, literature_note, managed_note, merge_properties, note_path, safe_path, safe_text,
                   split_frontmatter, task_lines, task_properties, update_line)
from .google_calendar import GoogleCalendar

log = logging.getLogger("hub")


def now():
    return dt.datetime.now(dt.timezone.utc).isoformat()


def secret(name):
    override = Path(os.environ.get("HUB_SETTINGS_DIR","/state/configuration")) / "secrets" / name
    path = override if override.exists() else Path(os.environ.get("HUB_SECRETS", "/run/secrets")) / name
    if not path.exists() or not path.read_text().strip():
        raise NotConfigured(f"Configure secret file: {name}")
    return path.read_text().strip()


class Service:
    def __init__(self, config, state, client=None):
        self.config = config
        self.state = state
        self.client = client or httpx.AsyncClient(timeout=httpx.Timeout(30, connect=5), follow_redirects=False)
        self.lock = asyncio.Lock()
        self.zone = ZoneInfo(config["timezone"])
        self.filter_cache = None
        self.zotero_validated_key = None
        self.search_cache = {}
        self.search_locks = {"zotero": asyncio.Lock(), "readeck": asyncio.Lock()}
        self.extension = None
        if os.environ.get("HUB_EXTENSION"):
            import importlib
            self.extension = importlib.import_module(os.environ["HUB_EXTENSION"])
            self.extension.install(self)
        self.google_calendar = GoogleCalendar(self)

    async def search_data(self, source, term, fetch):
        """Bounded 60s cache, coalesced requests; no search strings in logs."""
        key = (source, term.strip().casefold())
        stamp = asyncio.get_running_loop().time()
        cached = self.search_cache.get(key)
        if cached and stamp - cached[0] < 60:
            return cached[1]
        async with self.search_locks[source]:
            stamp = asyncio.get_running_loop().time()
            cached = self.search_cache.get(key)
            if cached and stamp - cached[0] < 60:
                return cached[1]
            data = await fetch()
            if len(self.search_cache) >= 64:
                self.search_cache.pop(next(iter(self.search_cache)))
            self.search_cache[key] = (stamp, data)
            return data

    async def request(self, method, url, headers=None, retries=True, **kwargs):
        for attempt in range(4):
            try:
                response = await self.client.request(method, url, headers=headers, **kwargs)
            except (httpx.TimeoutException, httpx.NetworkError):
                if not retries:
                    raise Uncertain("Remote write outcome unknown") from None
                if attempt == 3:
                    raise
                await asyncio.sleep(2 ** attempt + random.uniform(0, 0.5))
                continue
            if response.status_code == 412:
                raise Conflict("File changed while synchronizing; retry after rereading")
            if retries and response.status_code in (429, 502, 503, 504) and attempt < 3:
                wait = response.headers.get("Retry-After", response.headers.get("Backoff", str(2 ** attempt)))
                await asyncio.sleep(float(wait) if wait.isdigit() else 2 ** attempt + random.uniform(0, 0.5))
                continue
            if response.status_code == 304:
                return response
            response.raise_for_status()
            if response.headers.get("Backoff", "").isdigit():
                await asyncio.sleep(float(response.headers["Backoff"]))
            return response
        raise RuntimeError("Request retry exhausted")

    def sb_headers(self):
        return {"Authorization": "Bearer " + secret("silverbullet_token"), "X-Sync-Mode": "true"}

    async def files(self):
        r = await self.request("GET", self.config["silverbullet_url"] + "/.fs", headers=self.sb_headers())
        data = r.json()
        if isinstance(data, dict):
            return list(data)
        return [x if isinstance(x, str) else x.get("name", x.get("path")) for x in data]

    async def read(self, path):
        path = safe_path(path)
        try:
            r = await self.request("GET", self.config["silverbullet_url"] + "/.fs/" + quote(path, safe="/"), headers=self.sb_headers())
        except httpx.HTTPStatusError as e:
            if e.response.status_code == 404:
                return None, None
            raise
        return r.text, r.headers.get("etag")

    async def write(self, path, text, etag=None):
        headers = self.sb_headers()
        headers["Content-Type"] = "text/markdown; charset=utf-8"
        if etag:
            headers["If-Match"] = etag
        else:
            headers["If-None-Match"] = "*"
        return await self.request("PUT", self.config["silverbullet_url"] + "/.fs/" + quote(safe_path(path), safe="/"),
                                  headers=headers, content=text.encode(), retries=False)

    async def vk(self, method, path, **kwargs):
        return await self.request(method, self.config["vikunja"]["url"] + path,
                                  headers={"Authorization": "Bearer " + secret("vikunja_token")}, **kwargs)

    async def paginated_vk(self, path, params=None):
        result, page = [], 1
        while True:
            r = await self.vk("GET", path, params={**(params or {}), "page": page, "per_page": 100})
            data = r.json() or []
            result.extend(data)
            pages = int(r.headers.get("x-pagination-total-pages", page))
            if page >= pages:
                return result
            page += 1

    async def update_task(self, task_id, patch):
        current = (await self.vk("GET", f"/tasks/{task_id}")).json()
        task = dict(current)
        task.update({k: v for k, v in patch.items() if k != "labels"})
        if "due_date" in patch and re.fullmatch(r"\d{4}-\d{2}-\d{2}", patch["due_date"] or ""):
            task["due_date"] = dt.datetime.fromisoformat(patch["due_date"]).replace(tzinfo=self.zone).isoformat()
        await self.vk("POST", f"/tasks/{task_id}", json=task, retries=False)
        if "labels" in patch:
            wanted = set(patch["labels"])
            existing = {x["id"] for x in current.get("labels") or []}
            for label in wanted - existing:
                await self.vk("PUT", f"/tasks/{task_id}/labels", json={"label_id": label}, retries=False)
            for label in existing - wanted:
                await self.vk("DELETE", f"/tasks/{task_id}/labels/{label}", retries=False)
        self.filter_cache = None
        return (await self.vk("GET", f"/tasks/{task_id}")).json()

    async def find_origin(self, origin):
        # Server search narrows candidates, but identity is checked against the UUID marker.
        tasks = await self.paginated_vk("/tasks", {"s": origin})
        matches = [t for t in tasks if f"SilverBullet origin: {origin}" in (t.get("description") or "")]
        if len(matches) > 1:
            raise Conflict("Duplicate origin found in Vikunja")
        return matches[0] if matches else None

    async def reconcile_daily(self, path):
        if not DAILY.fullmatch(path):
            raise ValueError("Only Daily/YYYY-MM-DD.md is eligible")
        async with self.lock:
            text, etag = await self.read(path)
            if text is None:
                return {"processed": 0}
            parsed = task_lines(text)
            lines = text.splitlines()
            original_lines = list(lines)
            for line in parsed:
                lines[line.index] = line.render()
            if lines != original_lines:
                # Persist UUID before making any remote create request.
                await self.write(path, "\n".join(lines) + ("\n" if text.endswith("\n") else ""), etag)
            processed = 0
            for line in parsed:
                origin = line.meta["origin"]
                record = self.state.task(origin)
                task_id = line.meta.get("id") or (record or {}).get("id")
                base = (record or {}).get("base")
                if task_id:
                    remote = (await self.vk("GET", f"/tasks/{task_id}")).json()
                    remote_props = task_properties(remote)
                    if not base:
                        # A lost state DB is reconstructed from canonical remote IDs.
                        merged = remote_props
                    else:
                        merged = merge_properties(base, line.properties(self.config["vikunja"]["inbox_id"]), remote_props)
                    if merged != remote_props:
                        remote = await self.update_task(task_id, merged)
                else:
                    if record and record["phase"] == "uncertain":
                        remote = await self.find_origin(origin)
                        if not remote:
                            raise Uncertain("Create may have succeeded; reconcile before retrying")
                    else:
                        project = line.meta.get("project", self.config["vikunja"]["inbox_id"])
                        await self.vk("GET", f"/projects/{project}")
                        self.state.save_task(origin, None, "uncertain", None, path)
                        body = {"title": line.title, "done": line.done,
                                "description": f"<p>SilverBullet origin: {origin}</p>"}
                        for k in ("due_date", "priority"):
                            body[k] = line.properties(project)[k]
                        try:
                            remote = (await self.vk("PUT", f"/projects/{project}/tasks", json=body, retries=False)).json()
                        except httpx.HTTPStatusError as e:
                            if 400 <= e.response.status_code < 500:
                                self.state.save_task(origin, None, "rejected", None, path, "Request rejected")
                            raise
                        if line.meta.get("labels"):
                            remote = await self.update_task(remote["id"], {"labels": line.meta["labels"]})
                    task_id = remote["id"]
                props = task_properties(remote)
                self.state.save_task(origin, task_id, "synced", props, path)
                # Read again: a person may have edited this file during the upstream request.
                latest, latest_etag = await self.read(path)
                updated = task_lines(latest)
                matches = [x for x in updated if x.meta["origin"] == origin]
                for target in matches:
                    if target.title != line.title or target.done != line.done or target.meta != line.meta:
                        raise Conflict("Daily changed during synchronization; remote ID is retained in state")
                if matches:
                    out = latest.splitlines()
                    for target in matches:
                        target.meta["id"] = task_id
                        update_line(target, props)
                        out[target.index] = target.render()
                    if out != latest.splitlines():
                        await self.write(path, "\n".join(out) + ("\n" if latest.endswith("\n") else ""), latest_etag)
                processed += 1
            return {"processed": processed}

    async def saved_filter(self, force=False):
        stamp = asyncio.get_running_loop().time()
        if self.filter_cache and not force and stamp - self.filter_cache[0] < 30:
            return self.filter_cache[1]
        cfg = self.config["vikunja"]
        filt = (await self.vk("GET", f'/filters/{cfg["saved_filter_id"]}')).json()
        query = filt["filters"]
        params = {key: value for key, value in query.items() if key in
                  ("filter", "filter_timezone", "filter_include_nulls", "sort_by", "order_by", "s") and value is not None}
        params.setdefault("filter_timezone", self.config["timezone"])
        data = {"title": filt["title"], "updated": now(), "tasks": await self.paginated_vk("/tasks", params)}
        self.filter_cache = (stamp, data)
        return data

    async def tasks_today(self):
        today = dt.datetime.now(self.zone).date()
        start = dt.datetime.combine(today, dt.time.min, self.zone).isoformat()
        end = dt.datetime.combine(today + dt.timedelta(days=1), dt.time.min, self.zone).isoformat()
        tasks = await self.paginated_vk("/tasks", {"filter": f'done = false && due_date >= "{start}" && due_date < "{end}"',
                                                  "filter_timezone": self.config["timezone"]})
        return {"updated": now(), "tasks": tasks}

    async def zotero_user(self):
        key = secret("zotero_key")
        known = self.config["zotero"].get("user_id") or self.state.get("zotero_user")
        if self.zotero_validated_key != digest(key):
            r = await self.request("GET", "https://api.zotero.org/keys/current", headers={"Zotero-API-Key": key})
            data = r.json()
            access = data.get("access", {}).get("user", {})
            if not access.get("library") or not access.get("notes"):
                raise NotConfigured("Zotero key must permit reading library and notes")
            if known and int(known) != data["userID"]:
                raise NotConfigured("Zotero key belongs to another library; do not reuse existing mappings")
            known = data["userID"]
            self.state.set("zotero_user", known)
            self.zotero_validated_key = digest(key)
        return int(known)

    async def zt(self, path, params=None):
        user = await self.zotero_user()
        return await self.request("GET", f"https://api.zotero.org/users/{user}" + path, params=params,
                                  headers={"Zotero-API-Key": secret("zotero_key"), "Zotero-API-Version": "3"})

    async def zt_items(self, path, params=None):
        result, start = [], 0
        while True:
            r = await self.zt(path, {**(params or {}), "start": start, "limit": 100})
            data = r.json()
            result.extend(data)
            start += len(data)
            if not data or start >= int(r.headers.get("Total-Results", start)):
                return result

    async def zotero_search(self, query):
        async def fetch():
            return (await self.zt("/items/top", {"q": query.strip(), "qmode": "titleCreatorYear", "limit": 50, "format": "json"})).json()
        data = await self.search_data("zotero", query, fetch)
        return [{"key": x["key"], "title": x["data"].get("title", ""),
                 "authors": self.authors(x["data"]), "year": self.year(x["data"]),
                 "path": (self.state.note("ZOTERO", x["key"]) or {}).get("path")} for x in data]

    @staticmethod
    def authors(data):
        return [x.get("name") or " ".join(v for v in (x.get("firstName"), x.get("lastName")) if v)
                for x in data.get("creators", []) if x.get("creatorType") in ("author", "editor", "inventor")]

    @staticmethod
    def year(data):
        match = re.search(r"\b(\d{4})\b", data.get("date", ""))
        return int(match[1]) if match else None

    async def citekey(self, data):
        user = await self.zotero_user()
        text, etag = await self.read("Bibliography/citekeys.json")
        registry = json.loads(text) if text else {}
        identity = f"user:{user}:{data['key']}"
        if identity in registry:
            return registry[identity]
        key = citation_key(user, data)
        if key in registry.values():
            raise Conflict("Citation key collision; pin a distinct key in Zotero Extra")
        registry[identity] = key
        await self.write("Bibliography/citekeys.json", json.dumps(registry, indent=2, ensure_ascii=False) + "\n", etag)
        return key

    async def citation(self, key):
        if not re.fullmatch(r"[A-Z0-9]{8}", key):
            raise ValueError("Invalid Zotero item key")
        item = (await self.zt(f"/items/{key}", {"format": "json", "include": "data,bib,citation", "style": "apa", "locale": "pt-BR"})).json()
        data = item["data"]
        async with self.lock:
            citekey = await self.citekey(data)
            r = await self.zt(f"/items/{quote(key, safe='')}", {"format": "csljson"})
            entries = r.json()
            entry = entries[0] if isinstance(entries, list) else entries
            entry["id"] = citekey
            old, etag = await self.read("Bibliography/references.json")
            references = json.loads(old) if old else []
            references = [x for x in references if x["id"] != citekey] + [entry]
            await self.write("Bibliography/references.json", json.dumps(references, ensure_ascii=False, indent=2) + "\n", etag)
            bib = (await self.zt(f"/items/{quote(key, safe='')}", {"format": "biblatex"})).text
            bib = re.sub(r"(@[A-Za-z]+\s*\{)[^,]+,", lambda m: m[1] + citekey + ",", bib, count=1)
            # Each entry is a separate durable file, avoiding unsafe regex parsing of nested BibTeX braces.
            old_bib, bib_etag = await self.read(f"Bibliography/Entries/{citekey}.bib")
            if old_bib != bib:
                await self.write(f"Bibliography/Entries/{citekey}.bib", bib, bib_etag)
            reference, citation = apa_markdown(item.get("bib")), apa_markdown(item.get("citation"))
            if not reference or not citation:
                raise Conflict("Zotero did not return an APA reference")
            return {"citekey": citekey, "citation": citation, "reference": reference,
                    "identity": f"user:{await self.zotero_user()}:{key}", "pandocCitation": f"[@{citekey}]"}

    async def cite_zotero(self, key):
        record = self.state.note("ZOTERO", key)
        existing = (await self.read(record["path"]))[0] if record else None
        note = {"path": record["path"][:-3]} if existing is not None else await self.import_zotero(key)
        return {**await self.citation(key), **note}

    async def upsert_note(self, source, identity, path, meta, block):
        record = self.state.note(source, identity)
        if record:
            path = record["path"]
        else:
            # Recover mapping from frontmatter when the state DB was lost.
            field = "zoteroKey" if source == "ZOTERO" else "readeckId"
            for candidate in await self.files():
                if candidate and candidate.startswith("Literature/" if source == "ZOTERO" else "Reading/") and candidate.endswith(".md"):
                    content, _ = await self.read(candidate)
                    front, _ = split_frontmatter(content or "")
                    if front.get(field) == identity:
                        path = candidate
                        break
        for attempt in range(3):
            existing, etag = await self.read(path)
            if isinstance(block, dict):
                patched, saved_block = literature_note(existing, meta, block, record["hash"] if record else None)
            else:
                patched = managed_note(existing, meta, block, source, record["hash"] if record else None)
                saved_block = block
            try:
                if patched != existing:
                    await self.write(path, patched, etag)
                self.state.save_note(source, identity, path, saved_block)
                return {"path": path[:-3]}
            except Conflict:
                if attempt == 2:
                    raise

    async def import_zotero(self, key):
        if not re.fullmatch(r"[A-Z0-9]{8}", key):
            raise ValueError("Invalid Zotero item key")
        item = (await self.zt(f"/items/{key}", {"format": "json", "include": "data,bib,citation", "style": "apa", "locale": "pt-BR"})).json()
        data = item["data"]
        user = await self.zotero_user()
        children = await self.zt_items(f"/items/{key}/children")
        annotations, notes = [], []
        for child in children:
            c = child["data"]
            if c.get("itemType") == "note":
                notes.append(f"### Nota {child['key']}\n\n" + html_text(c.get("note")))
            elif c.get("itemType") == "attachment":
                descendants = await self.zt_items(f"/items/{child['key']}/children")
                for descendant in descendants:
                    a = descendant["data"]
                    if a.get("itemType") != "annotation":
                        continue
                    annotations.append(f"### Annotation {descendant['key']} · p. {safe_text(a.get('annotationPageLabel', ''))}\n\n"
                                       f"> {safe_text(a.get('annotationText', ''))}\n\n{safe_text(a.get('annotationComment', ''))}")
        collection_names = []
        for collection in data.get("collections", []):
            c = (await self.zt(f"/collections/{collection}")).json()
            collection_names.append({"key": collection, "name": c["data"]["name"]})
        async with self.lock:
            citekey = await self.citekey(data)
            authors, year = self.authors(data), self.year(data)
            meta = {"type": "literature", "zoteroLibrary": f"user:{user}", "zoteroKey": key,
                    "zoteroVersion": item["version"], "citekey": citekey, "title": data.get("title", ""),
                    "authors": authors, "year": year, "doi": data.get("DOI", ""), "url": data.get("url", ""),
                    "abstract": data.get("abstractNote", ""), "tags": [x["tag"] for x in data.get("tags", [])],
                    "collections": collection_names}
            portuguese = re.search(r"(?mi)^Abstract\[pt(?:-BR)?\]:\s*(.+)$", data.get("extra", ""))
            abstract = portuguese[1] if portuguese else data.get("abstractNote", "")
            meta["abstractLanguage"] = "pt-BR" if portuguese else data.get("language", "")
            reference = apa_markdown(item.get("bib"))
            if not reference:
                raise Conflict("Zotero did not return an APA bibliography")
            block = {"REFERENCE": reference,
                     "TAGS": ", ".join(safe_text(t) for t in meta["tags"]) or "Sem tags no Zotero.",
                     "ABSTRACT": safe_text(abstract) or "Sem abstract disponível no Zotero.",
                     "NOTES": "\n\n".join(notes + annotations) or "Sem notas ou annotations no Zotero."}
            return await self.upsert_note("ZOTERO", key, note_path("Literature", f"{authors[0] if authors else 'Sem autor'} {year or 's.d.'} - {data.get('title', '')}", key), meta, block)

    async def sync_zotero(self):
        checkpoint = self.state.get("zotero_version", 0)
        response = await self.zt("/items", {"since": checkpoint, "format": "versions", "includeTrashed": 1})
        version = int(response.headers["Last-Modified-Version"])
        changed = response.json()
        known = [r[0] for r in self.state.db.execute("SELECT identity FROM notes WHERE source='ZOTERO'")]
        parents = set()
        for key in changed if known else []:
            item = (await self.zt(f"/items/{key}")).json()["data"]
            parent = item.get("parentItem")
            if parent:
                obj = (await self.zt(f"/items/{parent}")).json()["data"]
                parent = obj.get("parentItem", parent)
            if key in known:
                parents.add(key)
            elif parent in known:
                parents.add(parent)
        for key in parents:
            await self.import_zotero(key)
        registry_text, _ = await self.read("Bibliography/citekeys.json")
        registry = json.loads(registry_text) if registry_text else {}
        for identity in registry:
            key = identity.rsplit(":", 1)[-1]
            if key in changed:
                await self.citation(key)
        deleted = await self.zt("/deleted", {"since": checkpoint})
        if int(deleted.headers["Last-Modified-Version"]) != version:
            raise Conflict("Zotero changed during sync; checkpoint retained")
        for key in deleted.json().get("items", []):
            if key in known:
                self.state.set("zotero_removed:" + key, now())
        self.state.set("zotero_version", version)

    async def rd(self, method, path, **kwargs):
        return await self.request(method, self.config["readeck"]["url"] + path,
                                  headers={"Authorization": "Bearer " + secret("readeck_token")}, **kwargs)

    async def readeck_search(self, query):
        term = query.strip()
        # Readeck uses whole-token search unless the token ends with '*'.
        # Preserve explicit advanced syntax instead of rewriting operators.
        search = " ".join(word + "*" for word in term.split()) if re.fullmatch(r"[\w\s]+", term) else term
        async def fetch():
            return (await self.rd("GET", "/bookmarks", params={"search": search, "limit": 50})).json()
        data = await self.search_data("readeck", query, fetch)
        return [{"id": x["id"], "title": x.get("title", ""), "url": x.get("url", ""),
                 "path": (self.state.note("READECK", x["id"]) or {}).get("path")} for x in data]

    async def import_readeck(self, bookmark):
        uid = bookmark["id"]
        annotations = (await self.rd("GET", f"/bookmarks/{uid}/annotations")).json()
        block = "\n\n".join(f"### Highlight {a.get('id', '')}\n\n> {html_text(a.get('text', a.get('content', '')))}\n\n"
                            + html_text(a.get("note", a.get("comment", ""))) for a in annotations)
        if bookmark.get("note"):
            block = "### Nota do bookmark\n\n" + html_text(bookmark["note"]) + "\n\n" + block
        meta = {"type": "reading", "readeckId": uid, "title": bookmark.get("title", ""),
                "url": bookmark.get("url", ""), "authors": bookmark.get("authors", []),
                "created": bookmark.get("created"), "published": bookmark.get("published"),
                "labels": bookmark.get("labels", [])}
        async with self.lock:
            result = await self.upsert_note("READECK", uid, note_path("Reading", bookmark.get("title"), uid), meta, block)
        labels = bookmark.get("labels") or []
        triggers = self.config["readeck"]["trigger_labels"]
        if any(x in labels for x in triggers):
            # Re-read before changing labels to preserve labels added during note import.
            latest = (await self.rd("GET", f"/bookmarks/{uid}")).json()
            labels = latest.get("labels") or []
            wanted = [x for x in labels if x not in triggers]
            processed = self.config["readeck"]["processed_label"]
            if processed not in wanted:
                wanted.append(processed)
            await self.rd("PATCH", f"/bookmarks/{uid}", json={"labels": wanted}, retries=False)
        return result

    async def import_readeck_id(self, uid):
        if not re.fullmatch(r"[A-Za-z0-9_-]{1,100}", uid):
            raise ValueError("Invalid Readeck bookmark ID")
        bookmark = (await self.rd("GET", f"/bookmarks/{uid}")).json()
        return await self.import_readeck(bookmark)

    async def sync_readeck(self):
        cursor = self.state.get("readeck_cursor")
        started = now()
        params = {"since": cursor} if cursor else {}
        updates = (await self.rd("GET", "/bookmarks/sync", params=params)).json()
        known = {r[0] for r in self.state.db.execute("SELECT identity FROM notes WHERE source='READECK'")}
        ids = {x["id"] for x in updates if x.get("type") != "delete"}
        if self.state.get("readeck_full_day") != str(dt.date.today()):
            ids |= known
        for uid in ids:
            try:
                bookmark = (await self.rd("GET", f"/bookmarks/{uid}")).json()
            except httpx.HTTPStatusError as e:
                if e.response.status_code == 404:
                    self.state.set("readeck_removed:" + uid, now())
                    continue
                raise
            if uid in known or any(x in self.config["readeck"]["trigger_labels"] for x in bookmark.get("labels") or []):
                await self.import_readeck(bookmark)
        for item in updates:
            if item.get("type") == "delete" and item["id"] in known:
                self.state.set("readeck_removed:" + item["id"], now())
        self.state.set("readeck_cursor", (dt.datetime.fromisoformat(started) - dt.timedelta(seconds=2)).isoformat())
        self.state.set("readeck_full_day", str(dt.date.today()))

    async def run_job(self, name, function):
        try:
            await function()
            self.state.set("job:" + name, {"last_success": now(), "error": None})
        except Exception as exc:
            # Exception bodies can contain personal content or credentials: log type only.
            message = str(exc) if isinstance(exc, (Conflict, NotConfigured, Uncertain)) else type(exc).__name__
            self.state.set("job:" + name, {"last_success": self.state.get("job:" + name, {}).get("last_success"),
                                          "error": message, "last_error": now()})
            log.warning(json.dumps({"job": name, "error": message}))

    async def daily_job(self):
        for path in await self.files():
            if path and DAILY.fullmatch(path):
                await self.reconcile_daily(path)

    async def open_daily(self):
        day = dt.datetime.now(self.zone).date().isoformat()
        path = f"Daily/{day}.md"
        existing, _ = await self.read(path)
        if existing is None:
            content = (f"---\ntype: daily\ndate: {day}\n---\n\n# {day}\n\n"
                       "## Diário\n\n")
            try:
                # If-None-Match prevents replacing a note created concurrently.
                await self.write(path, content)
            except Conflict:
                if (await self.read(path))[0] is None:
                    raise
        return {"path": path[:-3]}

    async def worker(self):
        last = {}
        while True:
            for name, interval, job in (("daily", self.config["daily_interval"], self.daily_job),
                                        ("zotero", self.config["zotero_interval"], self.sync_zotero),
                                        ("readeck", self.config["readeck_interval"], self.sync_readeck)):
                if not self.config.get("enabled", {}).get(name, True):
                    continue
                stamp = asyncio.get_running_loop().time()
                if stamp - last.get(name, -10000) >= interval:
                    await self.run_job(name, job)
                    last[name] = stamp
            if self.config.get("google_calendar", {}).get("enabled"):
                stamp = asyncio.get_running_loop().time()
                interval = max(60, self.config["google_calendar"].get("interval", 300))
                if stamp - last.get("google_calendar", -10000) >= interval:
                    await self.run_job("google_calendar", self.google_calendar.run)
                    last["google_calendar"] = stamp
            if self.extension:
                await self.extension.tick(self, last)
            await asyncio.sleep(5)

    def status(self):
        git_status = Path("/backup-status/status.json")
        return {"jobs": {name: self.state.get("job:" + name, {}) for name in (("daily", "zotero", "readeck", "google_calendar") + getattr(self.extension,"job_names",()))},
                "google_calendar": {"enabled": self.config.get("google_calendar", {}).get("enabled", False),
                                    "authorized": (self.google_calendar.folder / "oauth.json").exists(),
                                    "calendar_configured": (self.google_calendar.folder / "calendar.json").exists(),
                                    **self.state.get("calendar:summary", {})},
                "uncertain_tasks": self.state.db.execute("SELECT count(*) FROM tasks WHERE phase='uncertain'").fetchone()[0],
                "git": json.loads(git_status.read_text()) if git_status.exists() else {"error": "Backup status unavailable"}}
