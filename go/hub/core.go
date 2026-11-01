package hub

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var DailyPattern = regexp.MustCompile(`^Daily/\d{4}-\d{2}-\d{2}\.md$`)
var checkbox = regexp.MustCompile(`^(\s*(?:[-+*]|\d+[.)])\s+)\[([ xX])\]\s+(.+)$`)
var marker = regexp.MustCompile(`<!-- sb-task:(\{.*?\}) -->`)
var originPattern = regexp.MustCompile(`^[0-9a-f-]{36}$`)

func Digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func SafePath(p string) (string, error) {
	if strings.HasPrefix(p, "/") {
		return "", invalid("Unsafe path")
	}
	for _, s := range strings.Split(p, "/") {
		if strings.HasPrefix(s, ".") {
			return "", invalid("Unsafe path")
		}
	}
	return p, nil
}
func SafeText(v any) string {
	return strings.NewReplacer("${", "&#36;{", "`", "\\`", "<", "&lt;", ">", "&gt;").Replace(str(v))
}
func htmlText(v string, apa bool) string {
	z := html.NewTokenizer(strings.NewReader(v))
	var b strings.Builder
	skip := 0
	for {
		t := z.Next()
		if t == html.ErrorToken {
			break
		}
		switch t {
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			tok := z.Token()
			tag := tok.Data
			end := t == html.EndTagToken
			if !end && (tag == "script" || tag == "style") {
				skip++
			}
			if end && skip == 0 && apa && (tag == "i" || tag == "em") {
				b.WriteByte('*')
			}
			if tag == "p" || tag == "div" || tag == "li" || (!end && tag == "br") {
				b.WriteByte('\n')
			}
			if end && (tag == "script" || tag == "style") && skip > 0 {
				skip--
			}
			if !end && skip == 0 && apa && (tag == "i" || tag == "em") {
				b.WriteByte('*')
			}
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
			}
		}
	}
	s := strings.TrimSpace(b.String())
	if apa {
		s = strings.Join(strings.Fields(s), " ")
	}
	return SafeText(s)
}
func HTMLText(s string) string    { return htmlText(s, false) }
func APAMarkdown(s string) string { return htmlText(s, true) }

// PythonJSON matches json.dumps(..., sort_keys=True, ensure_ascii=False), including spaces.
func PythonJSON(v any) (string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	var data any
	if e = json.Unmarshal(b, &data); e != nil {
		return "", e
	}
	var emit func(any) string
	emit = func(v any) string {
		switch x := v.(type) {
		case map[string]any:
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			parts := []string{}
			for _, k := range keys {
				parts = append(parts, emit(k)+": "+emit(x[k]))
			}
			return "{" + strings.Join(parts, ", ") + "}"
		case []any:
			parts := []string{}
			for _, v := range x {
				parts = append(parts, emit(v))
			}
			return "[" + strings.Join(parts, ", ") + "]"
		default:
			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			enc.SetEscapeHTML(false)
			_ = enc.Encode(v)
			return strings.TrimSuffix(out.String(), "\n")
		}
	}
	return emit(data), nil
}
func BlockText(v any) (string, error) {
	if s, ok := v.(string); ok {
		return strings.Trim(s, "\r\n"), nil
	}
	m := obj(v)
	r := Object{}
	for k, v := range m {
		r[k] = strings.Trim(str(v), "\r\n")
	}
	if x, ok := v.(map[string]string); ok {
		for k, v := range x {
			r[k] = strings.Trim(v, "\r\n")
		}
	}
	return PythonJSON(r)
}
func SplitFrontmatter(s string) (Object, string, error) {
	re := regexp.MustCompile(`(?s)\A---\r?\n(.*?)\r?\n---(?:\r?\n|$)`)
	ix := re.FindStringSubmatchIndex(s)
	if ix == nil {
		return Object{}, s, nil
	}
	m := Object{}
	if e := yaml.Unmarshal([]byte(s[ix[2]:ix[3]]), &m); e != nil {
		return nil, "", Conflict("Invalid frontmatter")
	}
	return m, s[ix[1]:], nil
}
func frontText(front Object, body string) (string, error) {
	b, e := yaml.Marshal(front)
	if e != nil {
		return "", e
	}
	return "---\n" + strings.TrimSpace(string(b)) + "\n---\n" + body, nil
}
func ManagedNote(existing *string, meta Object, block, source, hash string) (string, error) {
	block = strings.Trim(block, "\r\n")
	start, end := "<!-- "+source+":START -->", "<!-- "+source+":END -->"
	front := clone(meta)
	body := "\n## " + strings.Title(strings.ToLower(source)) + " annotations\n" + start + "\n" + block + "\n" + end + "\n\n## Minhas notas\n\n"
	if existing != nil {
		var e error
		front, body, e = SplitFrontmatter(*existing)
		if e != nil {
			return "", e
		}
		if strings.Count(body, start) != 1 || strings.Count(body, end) != 1 || strings.Index(body, start) >= strings.Index(body, end) {
			return "", Conflict("Managed section markers missing or ambiguous")
		}
		l, r := strings.Index(body, start)+len(start), strings.Index(body, end)
		raw := body[l:r]
		old := strings.Trim(raw, "\r\n")
		legacy := raw
		if strings.HasPrefix(raw, "\n") && strings.HasSuffix(raw, "\n") {
			legacy = raw[1 : len(raw)-1]
		}
		if hash != "" && hash != Digest(old) && hash != Digest(legacy) {
			return "", Conflict("Managed section was edited manually")
		}
		body = body[:l] + "\n" + block + "\n" + body[r:]
		for k, v := range meta {
			front[k] = v
		}
	}
	return frontText(front, body)
}
func LiteratureNote(existing *string, meta Object, sections map[string]string, hash string) (string, map[string]string, error) {
	sec := map[string]string{}
	for k, v := range sections {
		sec[k] = strings.Trim(v, "\r\n")
	}
	s := ""
	if existing != nil {
		s = *existing
	}
	front, body, e := SplitFrontmatter(s)
	if e != nil {
		return "", nil, e
	}
	meta = clone(meta)
	old := array(front["zoteroTags"])
	if _, ok := front["zoteroTags"]; !ok {
		old = array(front["tags"])
	}
	tags := append([]any{}, array(meta["tags"])...)
	for _, v := range array(front["tags"]) {
		if !contains(old, v) && !contains(tags, v) {
			tags = append(tags, v)
		}
	}
	meta["zoteroTags"] = array(meta["tags"])
	meta["tags"] = tags
	if str(front["abstractPt"]) != "" {
		sec["ABSTRACT"] = SafeText(front["abstractPt"])
		meta["abstractLanguage"] = "pt-BR"
	}
	region := func(k, v string) string {
		return "<!-- ZOTERO:" + k + ":START -->\n" + v + "\n<!-- ZOTERO:" + k + ":END -->"
	}
	if strings.Contains(body, "<!-- ZOTERO:REFERENCE:START -->") {
		previous := map[string]string{}
		for k := range sec {
			start, end := "<!-- ZOTERO:"+k+":START -->", "<!-- ZOTERO:"+k+":END -->"
			if strings.Count(body, start) != 1 || strings.Count(body, end) != 1 || strings.Index(body, start) >= strings.Index(body, end) {
				return "", nil, Conflict("Literature section markers missing or ambiguous")
			}
			previous[k] = strings.Trim(body[strings.Index(body, start)+len(start):strings.Index(body, end)], "\r\n")
		}
		b, _ := BlockText(previous)
		if hash != "" && Digest(b) != hash {
			return "", nil, Conflict("Imported Zotero section was edited; manual edits preserved")
		}
		for k, v := range sec {
			start, end := "<!-- ZOTERO:"+k+":START -->", "<!-- ZOTERO:"+k+":END -->"
			l, r := strings.Index(body, start)+len(start), strings.Index(body, end)
			body = body[:l] + "\n" + v + "\n" + body[r:]
		}
	} else {
		preserved := body
		if existing != nil {
			if _, e = ManagedNote(existing, Object{}, "", "ZOTERO", hash); e != nil {
				return "", nil, e
			}
			preserved = regexp.MustCompile(`(?s)<!-- ZOTERO:START -->.*?<!-- ZOTERO:END -->`).ReplaceAllString(body, "")
		}
		body = "\n" + region("REFERENCE", sec["REFERENCE"]) + "\n\n## Tags\n" + region("TAGS", sec["TAGS"]) + "\n\n### Minhas tags\n\n## Abstract / Resumo\n" + region("ABSTRACT", sec["ABSTRACT"]) + "\n\n## Notas\n\n### Importadas do Zotero\n" + region("NOTES", sec["NOTES"]) + "\n\n### Minhas notas\n\n## Mais coisas\n\n" + preserved
	}
	for k, v := range meta {
		front[k] = v
	}
	out, e := frontText(front, body)
	return out, sec, e
}
func contains(a []any, v any) bool {
	for _, x := range a {
		if equal(x, v) {
			return true
		}
	}
	return false
}
func equal(a, b any) bool { x, _ := json.Marshal(a); y, _ := json.Marshal(b); return bytes.Equal(x, y) }
func MergeProperties(base, local, remote Object) (Object, error) {
	m := clone(remote)
	for k, v := range local {
		if equal(v, base[k]) {
			continue
		}
		if !equal(remote[k], base[k]) && !equal(v, remote[k]) {
			return nil, Conflict("Task field changed on both sides: " + k)
		}
		m[k] = v
	}
	return m, nil
}

type TaskLine struct {
	Index  int
	Prefix string
	Done   bool
	Title  string
	Meta   Object
}

func (t TaskLine) Render() string {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	_ = e.Encode(t.Meta)
	mark := " "
	if t.Done {
		mark = "x"
	}
	return t.Prefix + "[" + mark + "] " + t.Title + " <!-- sb-task:" + strings.TrimSpace(b.String()) + " -->"
}
func (t TaskLine) Properties(inbox int) Object {
	m := Object{"title": t.Title, "done": t.Done, "project_id": inbox, "due_date": "0001-01-01T00:00:00Z", "priority": 0, "labels": []any{}}
	for a, b := range map[string]string{"project": "project_id", "due": "due_date", "priority": "priority", "labels": "labels"} {
		if v, ok := t.Meta[a]; ok {
			m[b] = v
		}
	}
	return m
}
func (t *TaskLine) Update(p Object) {
	t.Title = str(p["title"])
	t.Done = boolean(p["done"])
	for a, b := range map[string]string{"project": "project_id", "due": "due_date", "priority": "priority", "labels": "labels"} {
		t.Meta[a] = p[b]
	}
}
func TaskLines(s string) ([]TaskLine, error) {
	src := []byte(s)
	blocked := make([]bool, len(src)+1)
	root := goldmark.DefaultParser().Parse(text.NewReader(src))
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if enter {
			switch n.Kind() {
			case ast.KindFencedCodeBlock, ast.KindCodeBlock, ast.KindHTMLBlock:
				for i := 0; i < n.Lines().Len(); i++ {
					seg := n.Lines().At(i)
					for j := seg.Start; j < seg.Stop; j++ {
						blocked[j] = true
					}
				}
			}
		}
		return ast.WalkContinue, nil
	})
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	front := len(lines) > 0 && strings.TrimSuffix(lines[0], "\r") == "---"
	offset := 0
	out := []TaskLine{}
	fence := ""
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")
		off := offset
		offset += len(lines[i]) + 1
		if front {
			if i > 0 && line == "---" {
				front = false
			}
			continue
		}
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, "~~~") {
			kind := trim[:3]
			if fence == "" {
				fence = kind
			} else if fence == kind {
				fence = ""
			}
			continue
		}
		if fence != "" || off < len(blocked) && blocked[off] {
			continue
		}
		m := checkbox.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		title := m[3]
		meta := Object{}
		if ix := marker.FindStringSubmatchIndex(title); ix != nil {
			if e := json.Unmarshal([]byte(title[ix[2]:ix[3]]), &meta); e != nil {
				return nil, invalid("Invalid task marker")
			}
			title = strings.TrimRight(title[:ix[0]], " \t")
		}
		if strings.TrimSpace(title) == "" {
			continue
		}
		if _, ok := meta["origin"]; !ok {
			b := make([]byte, 16)
			if _, e := io.ReadFull(rand.Reader, b); e != nil {
				return nil, e
			}
			b[6] = (b[6] & 15) | 64
			b[8] = (b[8] & 63) | 128
			meta["origin"] = fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
		}
		if !originPattern.MatchString(str(meta["origin"])) {
			return nil, Conflict("Invalid task origin")
		}
		out = append(out, TaskLine{i, m[1], strings.ToLower(m[2]) == "x", title, meta})
	}
	return out, nil
}
func TaskProperties(t Object) Object {
	labels := []int{}
	for _, v := range array(t["labels"]) {
		labels = append(labels, num(obj(v)["id"]))
	}
	sort.Ints(labels)
	due := str(t["due_date"])
	if due == "" {
		due = "0001-01-01T00:00:00Z"
	}
	return Object{"title": t["title"], "done": boolean(t["done"]), "project_id": t["project_id"], "due_date": due, "priority": num(t["priority"]), "labels": labels}
}
func NotePath(folder, title, id string) (string, error) {
	if title == "" {
		title = "Sem título"
	}
	title = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]`).ReplaceAllString(title, " ")
	title = strings.Trim(strings.Join(strings.Fields(title), " "), " .")
	r := []rune(title)
	if len(r) > 110 {
		title = string(r[:110])
	}
	if utf8.RuneCountInString(title) == 0 {
		title = "Sem título"
	}
	return SafePath(folder + "/" + title + " - " + id + ".md")
}
func CitationKey(user int, item Object) string {
	m := regexp.MustCompile(`(?mi)^Citation Key:\s*([A-Za-z0-9_:.+\-]+)\s*$`).FindStringSubmatch(str(item["extra"]))
	if m != nil {
		return m[1]
	}
	return fmt.Sprintf("z%d_%s", user, str(item["key"]))
}

var _ = reflect.DeepEqual
