"""Stable identities, safe Markdown patches and durable operation state."""
import hashlib
import json
import re
import sqlite3
import uuid
from dataclasses import dataclass
from html.parser import HTMLParser
from pathlib import PurePosixPath

import yaml
from markdown_it import MarkdownIt

DAILY = re.compile(r"^Daily/\d{4}-\d{2}-\d{2}\.md$")
MARKER = re.compile(r"<!-- sb-task:(\{.*?\}) -->")
CHECKBOX = re.compile(r"^(\s*(?:[-+*]|\d+[.)])\s+)\[([ xX])\]\s+(.+)$")


class Conflict(Exception):
    pass


class NotConfigured(Exception):
    pass


class Uncertain(Exception):
    pass


def digest(text):
    return hashlib.sha256(text.encode()).hexdigest()


def safe_path(path):
    p = PurePosixPath(path)
    if p.is_absolute() or ".." in p.parts or any(s.startswith(".") for s in p.parts):
        raise ValueError("Unsafe path")
    return path


def safe_text(text):
    # Imported bibliography/web content must never become executable Space Lua.
    return str(text or "").replace("${", "&#36;{").replace("`", "\\`").replace("<", "&lt;").replace(">", "&gt;")


class TextOnly(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.parts = []
        self.skip = 0

    def handle_starttag(self, tag, attrs):
        if tag in ("script", "style"):
            self.skip += 1
        if tag in ("p", "br", "div", "li"):
            self.parts.append("\n")

    def handle_endtag(self, tag):
        if tag in ("script", "style"):
            self.skip = max(0, self.skip - 1)
        if tag in ("p", "div", "li"):
            self.parts.append("\n")

    def handle_data(self, data):
        if not self.skip:
            self.parts.append(data)


def html_text(value):
    parser = TextOnly()
    parser.feed(value or "")
    return safe_text("".join(parser.parts).strip())


class BibliographyText(TextOnly):
    """Keep CSL italics, but never import HTML or executable expressions."""
    def handle_starttag(self, tag, attrs):
        super().handle_starttag(tag, attrs)
        if not self.skip and tag in ("i", "em"):
            self.parts.append("*")

    def handle_endtag(self, tag):
        if not self.skip and tag in ("i", "em"):
            self.parts.append("*")
        super().handle_endtag(tag)


def apa_markdown(value):
    parser = BibliographyText()
    parser.feed(value or "")
    return safe_text(" ".join("".join(parser.parts).split()))


def block_text(block):
    if isinstance(block, dict):
        return json.dumps({k: v.strip("\r\n") for k, v in block.items()}, sort_keys=True, ensure_ascii=False)
    return block.strip("\r\n")


def literature_note(existing, metadata, sections, expected_hash=None):
    sections = {k: v.strip("\r\n") for k, v in sections.items()}
    front, body = split_frontmatter(existing or "")
    old_tags = front.get("zoteroTags", front.get("tags", [])) or []
    manual_tags = [tag for tag in (front.get("tags", []) or []) if tag not in old_tags]
    metadata = dict(metadata)
    metadata["zoteroTags"] = list(metadata.get("tags", []))
    metadata["tags"] = list(dict.fromkeys(metadata["zoteroTags"] + manual_tags))
    if front.get("abstractPt"):
        # Optional local Portuguese abstract: never translate through a third party.
        sections["ABSTRACT"] = safe_text(front["abstractPt"])
        metadata["abstractLanguage"] = "pt-BR"
    def region(name, value):
        return f"<!-- ZOTERO:{name}:START -->\n{value}\n<!-- ZOTERO:{name}:END -->"
    has_regions = "<!-- ZOTERO:REFERENCE:START -->" in body
    if has_regions:
        previous = {}
        for name in sections:
            start, end = f"<!-- ZOTERO:{name}:START -->", f"<!-- ZOTERO:{name}:END -->"
            if body.count(start) != 1 or body.count(end) != 1 or body.index(start) >= body.index(end):
                raise Conflict("Literature section markers missing or ambiguous")
            previous[name] = body[body.index(start) + len(start):body.index(end)].strip("\r\n")
        if expected_hash and digest(block_text(previous)) != expected_hash:
            raise Conflict("Imported Zotero section was edited; manual edits preserved")
        for name, value in sections.items():
            start, end = f"<!-- ZOTERO:{name}:START -->", f"<!-- ZOTERO:{name}:END -->"
            left, right = body.index(start) + len(start), body.index(end)
            body = body[:left] + "\n" + value + "\n" + body[right:]
    else:
        preserved = body
        if existing:
            # Validate legacy block before migration and preserve ALL unowned text.
            managed_note(existing, {}, "", "ZOTERO", expected_hash)
            preserved = re.sub(r"<!-- ZOTERO:START -->.*?<!-- ZOTERO:END -->", "", body, flags=re.S)
        body = ("\n" + region("REFERENCE", sections["REFERENCE"]) + "\n\n## Tags\n" +
                region("TAGS", sections["TAGS"]) + "\n\n### Minhas tags\n\n" +
                "## Abstract / Resumo\n" + region("ABSTRACT", sections["ABSTRACT"]) +
                "\n\n## Notas\n\n### Importadas do Zotero\n" + region("NOTES", sections["NOTES"]) +
                "\n\n### Minhas notas\n\n## Mais coisas\n\n" + preserved)
    front.update(metadata)
    text = "---\n" + yaml.safe_dump(front, allow_unicode=True, sort_keys=False).strip() + "\n---\n" + body
    return text, sections


@dataclass
class TaskLine:
    index: int
    prefix: str
    done: bool
    title: str
    meta: dict

    def render(self):
        marker = json.dumps(self.meta, ensure_ascii=False, separators=(",", ":"))
        return f'{self.prefix}[{"x" if self.done else " "}] {self.title} <!-- sb-task:{marker} -->'

    def properties(self, inbox):
        return {"title": self.title, "done": self.done, "project_id": self.meta.get("project", inbox),
                "due_date": self.meta.get("due", "0001-01-01T00:00:00Z"),
                "priority": self.meta.get("priority", 0), "labels": self.meta.get("labels", [])}


def task_lines(text):
    blocked = set()
    tokens = MarkdownIt().parse(text)
    for token in tokens:
        if token.type in ("fence", "code_block", "html_block") and token.map:
            blocked.update(range(*token.map))
    lines = text.splitlines()
    if lines and lines[0] == "---":
        for i, line in enumerate(lines[1:], 1):
            blocked.add(i)
            if line == "---":
                break
        blocked.add(0)
    result = []
    for index, line in enumerate(lines):
        if index in blocked:
            continue
        match = CHECKBOX.match(line)
        if not match:
            continue
        title = match[3]
        marker = MARKER.search(title)
        meta = json.loads(marker[1]) if marker else {}
        if marker:
            title = title[:marker.start()].rstrip()
        if not title.strip():
            continue
        meta.setdefault("origin", str(uuid.uuid4()))
        if not re.fullmatch(r"[0-9a-f-]{36}", meta["origin"]):
            raise Conflict("Invalid task origin")
        result.append(TaskLine(index, match[1], match[2].lower() == "x", title, meta))
    return result


def task_properties(task):
    return {"title": task["title"], "done": bool(task.get("done")), "project_id": task["project_id"],
            "due_date": task.get("due_date") or "0001-01-01T00:00:00Z",
            "priority": task.get("priority", 0), "labels": sorted(x["id"] for x in (task.get("labels") or []))}


def merge_properties(base, local, remote):
    merged = dict(remote)
    for key in local:
        if local[key] == base.get(key):
            continue
        if remote.get(key) != base.get(key) and local[key] != remote.get(key):
            raise Conflict(f"Task field changed on both sides: {key}")
        merged[key] = local[key]
    return merged


def update_line(task, properties):
    task.title = properties["title"]
    task.done = properties["done"]
    for short, key in (("project", "project_id"), ("due", "due_date"), ("priority", "priority"), ("labels", "labels")):
        task.meta[short] = properties[key]


def split_frontmatter(text):
    match = re.match(r"\A---\r?\n(.*?)\r?\n---(?:\r?\n|$)", text, re.S)
    if not match:
        return {}, text
    data = yaml.safe_load(match[1]) or {}
    if not isinstance(data, dict):
        raise Conflict("Invalid frontmatter")
    return data, text[match.end():]


def managed_note(existing, metadata, block, source, expected_hash=None):
    block = block.strip("\r\n")
    start, end = f"<!-- {source}:START -->", f"<!-- {source}:END -->"
    if existing is None:
        body = f"\n## {source.title()} annotations\n{start}\n{block}\n{end}\n\n## Minhas notas\n\n"
        front = metadata
    else:
        front, body = split_frontmatter(existing)
        if body.count(start) != 1 or body.count(end) != 1 or body.index(start) >= body.index(end):
            raise Conflict("Managed section markers missing or ambiguous")
        left = body.index(start) + len(start)
        right = body.index(end)
        raw_block = body[left:right]
        old_block = raw_block.strip("\r\n")
        # Older state hashed the source's trailing newlines, while comparison
        # stripped them. Accept that exact legacy representation, not edits.
        legacy_block = raw_block[1:-1] if raw_block.startswith("\n") and raw_block.endswith("\n") else raw_block
        if expected_hash and expected_hash not in (digest(old_block), digest(legacy_block)):
            raise Conflict("Managed section was edited manually")
        body = body[:left] + "\n" + block + "\n" + body[right:]
        front.update(metadata)
    return "---\n" + yaml.safe_dump(front, allow_unicode=True, sort_keys=False).strip() + "\n---\n" + body


def note_path(folder, title, identity):
    title = re.sub(r'[\\/:*?"<>|\x00-\x1f]', " ", str(title or "Sem título"))
    title = re.sub(r"\s+", " ", title).strip(" .")[:110] or "Sem título"
    return safe_path(f"{folder}/{title} - {identity}.md")


def citation_key(user_id, item):
    explicit = re.search(r"(?mi)^Citation Key:\s*([A-Za-z0-9_:.+-]+)\s*$", item.get("extra", ""))
    return explicit[1] if explicit else f'z{user_id}_{item["key"]}'


class State:
    def __init__(self, path):
        self.db = sqlite3.connect(path)
        self.db.execute("PRAGMA journal_mode=WAL")
        self.db.executescript("""
        CREATE TABLE IF NOT EXISTS kv(key TEXT PRIMARY KEY,value TEXT NOT NULL);
        CREATE TABLE IF NOT EXISTS tasks(origin TEXT PRIMARY KEY,task_id INTEGER,phase TEXT NOT NULL,
          base TEXT,path TEXT,error TEXT);
        CREATE TABLE IF NOT EXISTS notes(source TEXT,identity TEXT,path TEXT,block_hash TEXT,
          PRIMARY KEY(source,identity));
        CREATE TABLE IF NOT EXISTS errors(key TEXT PRIMARY KEY,message TEXT,time TEXT);
        """)

    def get(self, key, default=None):
        row = self.db.execute("SELECT value FROM kv WHERE key=?", (key,)).fetchone()
        return json.loads(row[0]) if row else default

    def set(self, key, value):
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO kv VALUES(?,?)", (key, json.dumps(value)))

    def task(self, origin):
        row = self.db.execute("SELECT task_id,phase,base,path,error FROM tasks WHERE origin=?", (origin,)).fetchone()
        return {"id": row[0], "phase": row[1], "base": json.loads(row[2]) if row[2] else None,
                "path": row[3], "error": row[4]} if row else None

    def save_task(self, origin, task_id, phase, base, path, error=None):
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO tasks VALUES(?,?,?,?,?,?)",
                            (origin, task_id, phase, json.dumps(base), path, error))

    def note(self, source, identity):
        row = self.db.execute("SELECT path,block_hash FROM notes WHERE source=? AND identity=?", (source, identity)).fetchone()
        return {"path": row[0], "hash": row[1]} if row else None

    def save_note(self, source, identity, path, block):
        with self.db:
            self.db.execute("INSERT OR REPLACE INTO notes VALUES(?,?,?,?)", (source, identity, path, digest(block_text(block))))
