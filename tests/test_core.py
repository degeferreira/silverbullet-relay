import json
import unittest

from hub.core import (Conflict, State, apa_markdown, block_text, citation_key, digest, html_text, literature_note,
                      managed_note, merge_properties, safe_path, split_frontmatter, task_lines)


class MarkdownTests(unittest.TestCase):
    def test_identity_survives_title_change(self):
        task = task_lines("- [ ] Primeiro título")[0]
        original = task.meta["origin"]
        task.title = "Outro título"
        self.assertEqual(task_lines(task.render())[0].meta["origin"], original)

    def test_code_and_frontmatter_are_not_tasks(self):
        text = "---\nexample: '- [ ] x'\n---\n- [ ] real\n  - [x] nested\n```md\n- [ ] fake\n```\n"
        self.assertEqual([x.title for x in task_lines(text)], ["real", "nested"])

    def test_manual_section_is_preserved_byte_for_byte(self):
        original = managed_note(None, {"type": "literature"}, "old", "ZOTERO")
        original += "Meu texto  com espaços\n\n${minhaExpressao()}\n"
        expected = original.split("## Minhas notas", 1)[1]
        updated = managed_note(original, {"title": "novo"}, "new", "ZOTERO", digest("old"))
        self.assertEqual(updated.split("## Minhas notas", 1)[1], expected)

    def test_edited_managed_content_conflicts(self):
        original = managed_note(None, {}, "changed manually", "ZOTERO")
        with self.assertRaises(Conflict):
            managed_note(original, {}, "new", "ZOTERO", digest("old"))

    def test_broken_markers_conflict(self):
        with self.assertRaises(Conflict):
            managed_note("My note", {}, "new", "ZOTERO")

    def test_annotation_trailing_newlines_do_not_create_false_conflict(self):
        block = "### Annotation\n\n> Highlight\n\n"
        original = managed_note(None, {}, block, "ZOTERO") + "Manual note\n"
        state = State(":memory:")
        try:
            state.save_note("ZOTERO", "key", "Literature/test.md", block)
            updated = managed_note(original, {}, block, "ZOTERO", state.note("ZOTERO", "key")["hash"])
            self.assertTrue(updated.endswith("Manual note\n"))
        finally:
            state.db.close()

    def test_legacy_raw_annotation_hash_is_supported(self):
        block = "old\n\n"
        original = "---\n{}\n---\n<!-- ZOTERO:START -->\n" + block + "\n<!-- ZOTERO:END -->\n\n## Minhas notas\nmanual\n"
        updated = managed_note(original, {}, "new", "ZOTERO", digest(block))
        self.assertTrue(updated.endswith("manual\n"))

    def test_imported_content_cannot_execute(self):
        value = html_text('<p>${danger()}</p><script>secret()</script><p>```space-lua</p>')
        self.assertNotIn("secret()", value)
        self.assertNotIn("${", value)
        self.assertNotIn("```space-lua", value)

    def test_path_traversal(self):
        for path in ("../secrets/key", "/etc/passwd", ".git/config", "Notes/../../key"):
            with self.assertRaises(ValueError):
                safe_path(path)


class IdentityTests(unittest.TestCase):
    def test_key_does_not_depend_on_title(self):
        self.assertEqual(citation_key(123, {"key": "ABCD1234", "title": "A"}),
                         citation_key(123, {"key": "ABCD1234", "title": "B"}))

    def test_bbt_pinned_key(self):
        self.assertEqual(citation_key(123, {"key": "ABCD1234", "extra": "Citation Key: Silva2020"}), "Silva2020")

    def test_merge_distinct_fields(self):
        base = {"title": "A", "done": False}
        self.assertEqual(merge_properties(base, {"title": "B", "done": False}, {"title": "A", "done": True}),
                         {"title": "B", "done": True})

    def test_same_field_conflicts(self):
        with self.assertRaises(Conflict):
            merge_properties({"title": "A"}, {"title": "B"}, {"title": "C"})

    def test_durable_uncertain_creation(self):
        state = State(":memory:")
        state.save_task("uuid", None, "uncertain", None, "Daily/2026-10-02.md")
        self.assertEqual(state.task("uuid")["phase"], "uncertain")
        state.save_task("uuid", 123, "synced", {"title": "A"}, "Daily/2026-10-02.md")
        self.assertEqual(state.task("uuid")["id"], 123)


class LiteratureTests(unittest.TestCase):
    sections = {"REFERENCE": "Silva, A. (2020). *Livro*.", "TAGS": "pesquisa", "ABSTRACT": "English abstract.", "NOTES": "Highlights"}

    def test_layout_and_manual_fields_survive(self):
        text, saved = literature_note(None, {"tags": ["pesquisa"]}, self.sections)
        self.assertLess(text.index("Silva, A."), text.index("## Tags"))
        self.assertLess(text.index("## Tags"), text.index("## Abstract"))
        self.assertLess(text.index("## Abstract"), text.index("## Notas"))
        text = text.replace("### Minhas tags\n\n", "### Minhas tags\n\n#minha-tag\n\n")
        text = text.replace("### Minhas notas\n\n", "### Minhas notas\n\nMeu texto manual.\n\n")
        text = text.replace("- pesquisa\n", "- pesquisa\n- pessoal\n", 1)
        updated, saved2 = literature_note(text, {"tags": ["nova"]}, {**self.sections, "NOTES": "New highlights"}, digest(block_text(saved)))
        self.assertIn("#minha-tag", updated)
        self.assertIn("Meu texto manual.", updated)
        self.assertEqual(split_frontmatter(updated)[0]["tags"], ["nova", "pessoal"])
        self.assertEqual(literature_note(updated, {"tags": ["nova"]}, {**self.sections, "NOTES": "New highlights"}, digest(block_text(saved2)))[0], updated)

    def test_managed_edit_is_not_overwritten(self):
        text, saved = literature_note(None, {}, self.sections)
        text = text.replace("Highlights", "Manual edit to imported highlight")
        with self.assertRaises(Conflict):
            literature_note(text, {}, self.sections, digest(block_text(saved)))

    def test_optional_portuguese_abstract_is_preserved(self):
        text, saved = literature_note(None, {}, self.sections)
        text = text.replace("---\n", "---\nabstractPt: Resumo em português.\n", 1)
        updated, saved2 = literature_note(text, {}, self.sections, digest(block_text(saved)))
        self.assertEqual(saved2["ABSTRACT"], "Resumo em português.")
        self.assertEqual(split_frontmatter(updated)[0]["abstractLanguage"], "pt-BR")
        literature_note(updated, {}, self.sections, digest(block_text(saved2)))

    def test_legacy_migration_preserves_unowned_content(self):
        text = managed_note(None, {}, "old", "ZOTERO") + "Manual legacy text\n"
        updated, _ = literature_note(text, {}, self.sections, digest("old"))
        self.assertTrue(updated.endswith("Manual legacy text\n"))
        self.assertIn("### Minhas notas", updated)
        self.assertNotIn("<!-- ZOTERO:START -->", updated)

    def test_apa_html_is_safe_and_keeps_italics(self):
        text = apa_markdown('<div>Silva (2020). <i>Livro</i>. ${danger()}<script>secret</script></div>')
        self.assertIn("*Livro*", text)
        self.assertNotIn("${", text)
        self.assertNotIn("secret", text)


if __name__ == "__main__":
    unittest.main()
