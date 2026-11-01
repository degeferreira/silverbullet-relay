import copy
import json
import unittest
from unittest.mock import AsyncMock, patch

import httpx

from hub.core import State, Uncertain
from hub.service import Service


class IntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.config = {"timezone": "America/Sao_Paulo", "silverbullet_url": "http://sb",
                       "vikunja": {"url": "http://vk", "inbox_id": 1, "saved_filter_id": 1}}
        self.files = {"Daily/2026-10-02.md": "- [ ] Minha tarefa\n"}
        self.tasks = {}
        self.creates = 0
        self.fail_create = False

        def transport(request):
            path = request.url.path
            if request.url.host == "sb":
                if path == "/.fs":
                    return httpx.Response(200, json={p: {} for p in self.files})
                name = path.removeprefix("/.fs/")
                if request.method == "GET":
                    if name not in self.files:
                        return httpx.Response(404)
                    return httpx.Response(200, text=self.files[name], headers={"etag": "test"})
                self.files[name] = request.content.decode()
                return httpx.Response(200)
            if path == "/projects/1":
                return httpx.Response(200, json={"id": 1})
            if request.method == "PUT":
                self.creates += 1
                body = json.loads(request.content)
                body.update({"id": 123, "project_id": 1, "labels": []})
                self.tasks[123] = body
                if self.fail_create:
                    raise httpx.ReadTimeout("timeout", request=request)
                return httpx.Response(200, json=body)
            if path == "/tasks":
                return httpx.Response(200, json=list(self.tasks.values()))
            if path.startswith("/tasks/"):
                if request.method == "POST":
                    self.tasks[123].update(json.loads(request.content))
                return httpx.Response(200, json=self.tasks[123])
            return httpx.Response(404)

        self.client = httpx.AsyncClient(transport=httpx.MockTransport(transport))
        self.service = Service(self.config, State(":memory:"), self.client)
        self.secret = patch("hub.service.secret", return_value="fake-token")
        self.secret.start()

    async def asyncTearDown(self):
        self.secret.stop()
        await self.client.aclose()
        self.service.state.db.close()

    async def test_create_in_inbox_once_then_update(self):
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.assertEqual(self.creates, 1)
        self.assertEqual(self.tasks[123]["project_id"], 1)
        self.files["Daily/2026-10-02.md"] = self.files["Daily/2026-10-02.md"].replace("Minha tarefa", "Título editado").replace("[ ]", "[x]")
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.assertEqual(self.creates, 1)
        self.assertTrue(self.tasks[123]["done"])
        self.assertEqual(self.tasks[123]["title"], "Título editado")

    async def test_timeout_after_create_reconciles_uuid_without_duplicate(self):
        self.fail_create = True
        with self.assertRaises(Uncertain):
            await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.fail_create = False
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.assertEqual(self.creates, 1)
        self.assertIn('"id":123', self.files["Daily/2026-10-02.md"])

    async def test_missing_state_uses_remote_canonical_id(self):
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.service.state.db.close()
        self.service.state = State(":memory:")
        self.tasks[123]["title"] = "Mudou no Vikunja"
        await self.service.reconcile_daily("Daily/2026-10-02.md")
        self.assertEqual(self.creates, 1)
        self.assertIn("Mudou no Vikunja", self.files["Daily/2026-10-02.md"])

    async def test_daily_created_as_content_not_implementation(self):
        self.files.clear()
        result = await self.service.open_daily()
        content = self.files[result["path"] + ".md"]
        self.assertIn("## Diário", content)
        self.assertNotIn("## Tarefas", content)
        self.assertNotIn('hub.taskWidget', content)
        self.assertNotIn("```space-lua", content)

    async def test_open_daily_preserves_existing_notes(self):
        result = await self.service.open_daily()
        path = result["path"] + ".md"
        self.files[path] = "Meu diário manual\n"
        await self.service.open_daily()
        self.assertEqual(self.files[path], "Meu diário manual\n")


class BibliographyTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.service = Service({"timezone": "America/Sao_Paulo"}, State(":memory:"))
        self.service.zotero_user = AsyncMock(return_value=123)
        self.files = {}
        self.children_calls = 0
        async def read(path):
            return self.files.get(path), "test" if path in self.files else None
        async def write(path, text, etag=None):
            self.files[path] = text
        async def zt(path, params=None):
            if params and params.get("format") == "csljson":
                return httpx.Response(200, json=[{"id": "original", "title": "Book"}])
            if params and params.get("format") == "biblatex":
                return httpx.Response(200, text="@book{original, title={Book}}")
            if path == "/items/top":
                return httpx.Response(200, json=[{"key": "ABCD1234", "data": {"title": "Book"}}])
            return httpx.Response(200, json={"key": "ABCD1234", "version": 1,
                "data": {"key": "ABCD1234", "title": "Book", "date": "2020", "tags": [{"tag": "source"}],
                         "extra": "Abstract[pt-BR]: Resumo português.", "abstractNote": "English abstract.",
                         "creators": [{"creatorType": "author", "lastName": "Silva", "firstName": "Ana"}]},
                "bib": "<div>Silva, A. (2020). <i>Book</i>.</div>", "citation": "<span>(Silva, 2020)</span>"})
        async def children(*args):
            self.children_calls += 1
            return []
        self.service.read, self.service.write = read, write
        self.service.files = AsyncMock(side_effect=lambda: list(self.files))
        self.service.zt = AsyncMock(side_effect=zt)
        self.service.zt_items = children

    async def asyncTearDown(self):
        await self.service.client.aclose()
        self.service.state.db.close()

    async def test_cite_creates_link_target_only_once_with_apa(self):
        first = await self.service.cite_zotero("ABCD1234")
        self.assertEqual(first["citation"], "(Silva, 2020)")
        self.assertEqual(first["identity"], "user:123:ABCD1234")
        path = first["path"] + ".md"
        self.assertIn("Resumo português.", self.files[path])
        self.assertNotIn("English abstract.\n<!--", self.files[path])
        self.files[path] += "Manual note\n"
        second = await self.service.cite_zotero("ABCD1234")
        self.assertEqual(first["path"], second["path"])
        self.assertEqual(self.children_calls, 1)
        self.assertTrue(self.files[path].endswith("Manual note\n"))
        self.assertIn("Bibliography/Entries/z123_ABCD1234.bib", self.files)
        await self.service.import_zotero("ABCD1234")
        self.assertTrue(self.files[path].endswith("Manual note\n"))

    async def test_search_cache_keeps_current_note_mapping(self):
        await self.service.zotero_search("book")
        self.service.state.save_note("ZOTERO", "ABCD1234", "Literature/test.md", "text")
        second = await self.service.zotero_search("BOOK")
        self.assertEqual(second[0]["path"], "Literature/test.md")
        self.assertEqual(self.service.zt.await_count, 1)

    async def test_cache_is_bounded(self):
        for i in range(70):
            await self.service.zotero_search(str(i))
        self.assertEqual(len(self.service.search_cache), 64)

    async def test_invalid_item_key_rejected(self):
        with self.assertRaises(ValueError):
            await self.service.citation("../secrets")

    async def test_readeck_prefix_matching_and_advanced_search(self):
        self.service.rd = AsyncMock(return_value=httpx.Response(200, json=[]))
        await self.service.readeck_search("tele gram")
        self.assertEqual(self.service.rd.await_args.kwargs["params"]["search"], "tele* gram*")
        await self.service.readeck_search("title:telegram")
        self.assertEqual(self.service.rd.await_args.kwargs["params"]["search"], "title:telegram")
