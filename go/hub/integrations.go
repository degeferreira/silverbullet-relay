package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var zoteroKeyPattern = regexp.MustCompile(`^[A-Z0-9]{8}$`)

func (s *Service) ZoteroUser(ctx context.Context) (int, error) {
	key, e := s.Secret("zotero_key")
	if e != nil {
		return 0, e
	}
	known := num(obj(s.Config["zotero"])["user_id"])
	if known == 0 {
		v, e := s.State.Get("zotero_user", nil)
		if e != nil {
			return 0, e
		}
		known = num(v)
	}
	s.cacheMu.Lock()
	valid := s.zoteroValidated == Digest(key)
	s.cacheMu.Unlock()
	if !valid {
		v, e := s.requestJSON(ctx, "GET", "https://api.zotero.org/keys/current", http.Header{"Zotero-API-Key": {key}}, nil, nil, true)
		if e != nil {
			return 0, e
		}
		data := obj(v)
		access := obj(obj(data["access"])["user"])
		if !boolean(access["library"]) || !boolean(access["notes"]) {
			return 0, NotConfigured("Zotero key must permit reading library and notes")
		}
		id := num(data["userID"])
		if known != 0 && known != id {
			return 0, NotConfigured("Zotero key belongs to another library; do not reuse existing mappings")
		}
		known = id
		if e = s.State.Set("zotero_user", known); e != nil {
			return 0, e
		}
		s.cacheMu.Lock()
		s.zoteroValidated = Digest(key)
		s.cacheMu.Unlock()
	}
	return known, nil
}
func (s *Service) ztResponse(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	user, e := s.ZoteroUser(ctx)
	if e != nil {
		return nil, e
	}
	key, e := s.Secret("zotero_key")
	if e != nil {
		return nil, e
	}
	target := fmt.Sprintf("https://api.zotero.org/users/%d%s", user, path)
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	return s.Request(ctx, "GET", target, http.Header{"Zotero-API-Key": {key}, "Zotero-API-Version": {"3"}}, nil, true)
}
func (s *Service) ZT(ctx context.Context, path string, params url.Values) (any, error) {
	return decodeResponse(s.ztResponse(ctx, path, params))
}
func (s *Service) ZTItems(ctx context.Context, path string, params url.Values) ([]any, error) {
	out := []any{}
	for start := 0; start <= 1000000; {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("start", strconv.Itoa(start))
		p.Set("limit", "100")
		r, e := s.ztResponse(ctx, path, p)
		if e != nil {
			return nil, e
		}
		total, _ := strconv.Atoi(r.Header.Get("Total-Results"))
		v, e := decodeResponse(r, nil)
		if e != nil {
			return nil, e
		}
		data, ok := v.([]any)
		if !ok {
			return nil, invalid("Unsupported Zotero collection")
		}
		out = append(out, data...)
		start += len(data)
		if len(data) == 0 || total == 0 || start >= total {
			return out, nil
		}
	}
	return nil, invalid("Zotero pagination safety limit")
}
func Authors(data Object) []string {
	out := []string{}
	for _, v := range array(data["creators"]) {
		c := obj(v)
		kind := str(c["creatorType"])
		if kind != "author" && kind != "editor" && kind != "inventor" {
			continue
		}
		name := str(c["name"])
		if name == "" {
			name = strings.TrimSpace(str(c["firstName"]) + " " + str(c["lastName"]))
		}
		out = append(out, name)
	}
	return out
}
func Year(data Object) any {
	m := regexp.MustCompile(`\b(\d{4})\b`).FindStringSubmatch(str(data["date"]))
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	return n
}
func (s *Service) ZoteroSearch(ctx context.Context, q string) (any, error) {
	v, e := s.SearchData(ctx, "zotero", q, func() (any, error) {
		return s.ZT(ctx, "/items/top", url.Values{"q": {strings.TrimSpace(q)}, "qmode": {"titleCreatorYear"}, "limit": {"50"}, "format": {"json"}})
	})
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, v := range array(v) {
		x := obj(v)
		d := obj(x["data"])
		note, e := s.State.Note("ZOTERO", str(x["key"]))
		if e != nil {
			return nil, e
		}
		out = append(out, Object{"key": x["key"], "title": str(d["title"]), "authors": Authors(d), "year": Year(d), "path": note["path"]})
	}
	return out, nil
}
func (s *Service) Citekey(ctx context.Context, data Object) (string, error) {
	user, e := s.ZoteroUser(ctx)
	if e != nil {
		return "", e
	}
	text, tag, e := s.Read(ctx, "Bibliography/citekeys.json")
	if e != nil {
		return "", e
	}
	registry := map[string]string{}
	if text != nil {
		if e = json.Unmarshal([]byte(*text), &registry); e != nil {
			return "", e
		}
	}
	identity := fmt.Sprintf("user:%d:%s", user, str(data["key"]))
	if key, ok := registry[identity]; ok {
		return key, nil
	}
	key := CitationKey(user, data)
	for _, v := range registry {
		if v == key {
			return "", Conflict("Citation key collision; pin a distinct key in Zotero Extra")
		}
	}
	registry[identity] = key
	b, e := jsonPretty(registry)
	if e != nil {
		return "", e
	}
	if e = s.Write(ctx, "Bibliography/citekeys.json", b, tag); e != nil {
		return "", e
	}
	return key, nil
}
func jsonPretty(v any) (string, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	e := enc.Encode(v)
	return b.String(), e
}
func (s *Service) Citation(ctx context.Context, key string) (Object, error) {
	if !zoteroKeyPattern.MatchString(key) {
		return nil, invalid("Invalid Zotero item key")
	}
	v, e := s.ZT(ctx, "/items/"+key, url.Values{"format": {"json"}, "include": {"data,bib,citation"}, "style": {"apa"}, "locale": {"pt-BR"}})
	if e != nil {
		return nil, e
	}
	item := obj(v)
	s.Lock.Lock()
	defer s.Lock.Unlock()
	citekey, e := s.Citekey(ctx, obj(item["data"]))
	if e != nil {
		return nil, e
	}
	v, e = s.ZT(ctx, "/items/"+key, url.Values{"format": {"csljson"}})
	if e != nil {
		return nil, e
	}
	entry := obj(v)
	if a, ok := v.([]any); ok {
		if len(a) == 0 {
			return nil, Conflict("Zotero did not return CSL data")
		}
		entry = obj(a[0])
	}
	entry["id"] = citekey
	old, tag, e := s.Read(ctx, "Bibliography/references.json")
	if e != nil {
		return nil, e
	}
	refs := []any{}
	if old != nil {
		if e = json.Unmarshal([]byte(*old), &refs); e != nil {
			return nil, e
		}
	}
	out := []any{}
	for _, v := range refs {
		if str(obj(v)["id"]) != citekey {
			out = append(out, v)
		}
	}
	out = append(out, entry)
	text, e := jsonPretty(out)
	if e != nil {
		return nil, e
	}
	if e = s.Write(ctx, "Bibliography/references.json", text, tag); e != nil {
		return nil, e
	}
	r, e := s.ztResponse(ctx, "/items/"+key, url.Values{"format": {"biblatex"}})
	if e != nil {
		return nil, e
	}
	raw, e := io.ReadAll(r.Body)
	r.Body.Close()
	if e != nil {
		return nil, e
	}
	re := regexp.MustCompile(`(@[A-Za-z]+\s*\{)[^,]+,`)
	bib := string(raw)
	if ix := re.FindStringSubmatchIndex(bib); ix != nil {
		bib = bib[:ix[0]] + bib[ix[2]:ix[3]] + citekey + "," + bib[ix[1]:]
	}
	old, tag, e = s.Read(ctx, "Bibliography/Entries/"+citekey+".bib")
	if e != nil {
		return nil, e
	}
	if old == nil || *old != bib {
		if e = s.Write(ctx, "Bibliography/Entries/"+citekey+".bib", bib, tag); e != nil {
			return nil, e
		}
	}
	reference, citation := APAMarkdown(str(item["bib"])), APAMarkdown(str(item["citation"]))
	if reference == "" || citation == "" {
		return nil, Conflict("Zotero did not return an APA reference")
	}
	user, e := s.ZoteroUser(ctx)
	if e != nil {
		return nil, e
	}
	return Object{"citekey": citekey, "citation": citation, "reference": reference, "identity": fmt.Sprintf("user:%d:%s", user, key), "pandocCitation": "[@" + citekey + "]"}, nil
}
func (s *Service) CiteZotero(ctx context.Context, key string) (Object, error) {
	record, e := s.State.Note("ZOTERO", key)
	if e != nil {
		return nil, e
	}
	var existing *string
	if record != nil {
		existing, _, e = s.Read(ctx, str(record["path"]))
		if e != nil {
			return nil, e
		}
	}
	note := Object{}
	if existing != nil {
		note["path"] = strings.TrimSuffix(str(record["path"]), ".md")
	} else {
		note, e = s.ImportZotero(ctx, key)
		if e != nil {
			return nil, e
		}
	}
	result, e := s.Citation(ctx, key)
	if e != nil {
		return nil, e
	}
	for k, v := range note {
		result[k] = v
	}
	return result, nil
}
func (s *Service) UpsertNote(ctx context.Context, source, id, path string, meta Object, block any) (Object, error) {
	record, e := s.State.Note(source, id)
	if e != nil {
		return nil, e
	}
	if record != nil {
		path = str(record["path"])
	} else {
		field, prefix := "readeckId", "Reading/"
		if source == "ZOTERO" {
			field, prefix = "zoteroKey", "Literature/"
		}
		files, e := s.Files(ctx)
		if e != nil {
			return nil, e
		}
		for _, candidate := range files {
			if !strings.HasPrefix(candidate, prefix) || !strings.HasSuffix(candidate, ".md") {
				continue
			}
			content, _, e := s.Read(ctx, candidate)
			if e != nil {
				return nil, e
			}
			if content == nil {
				continue
			}
			front, _, e := SplitFrontmatter(*content)
			if e != nil {
				return nil, e
			}
			if str(front[field]) == id {
				path = candidate
				break
			}
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		existing, tag, e := s.Read(ctx, path)
		if e != nil {
			return nil, e
		}
		hash := str(record["hash"])
		var patched string
		var saved any = block
		if sec, ok := block.(map[string]string); ok {
			patched, saved, e = s.literature(existing, meta, sec, hash)
		} else {
			patched, e = ManagedNote(existing, meta, str(block), source, hash)
		}
		if e != nil {
			return nil, e
		}
		if existing == nil || patched != *existing {
			e = s.Write(ctx, path, patched, tag)
		}
		if e == nil {
			if e = s.State.SaveNote(source, id, path, saved); e != nil {
				return nil, e
			}
			return Object{"path": strings.TrimSuffix(path, ".md")}, nil
		}
		var conflict *Error
		if !errors.As(e, &conflict) || conflict.Status != 409 || attempt == 2 {
			return nil, e
		}
	}
	return nil, Conflict("Note update conflicted")
}
func (s *Service) literature(existing *string, meta Object, sec map[string]string, hash string) (string, any, error) {
	return LiteratureNote(existing, meta, sec, hash)
}
func (s *Service) ImportZotero(ctx context.Context, key string) (Object, error) {
	if !zoteroKeyPattern.MatchString(key) {
		return nil, invalid("Invalid Zotero item key")
	}
	v, e := s.ZT(ctx, "/items/"+key, url.Values{"format": {"json"}, "include": {"data,bib,citation"}, "style": {"apa"}, "locale": {"pt-BR"}})
	if e != nil {
		return nil, e
	}
	item := obj(v)
	data := obj(item["data"])
	user, e := s.ZoteroUser(ctx)
	if e != nil {
		return nil, e
	}
	children, e := s.ZTItems(ctx, "/items/"+key+"/children", nil)
	if e != nil {
		return nil, e
	}
	notes, annotations := []string{}, []string{}
	for _, v := range children {
		child := obj(v)
		c := obj(child["data"])
		switch str(c["itemType"]) {
		case "note":
			notes = append(notes, "### Nota "+str(child["key"])+"\n\n"+HTMLText(str(c["note"])))
		case "attachment":
			desc, e := s.ZTItems(ctx, "/items/"+str(child["key"])+"/children", nil)
			if e != nil {
				return nil, e
			}
			for _, v := range desc {
				d := obj(v)
				a := obj(d["data"])
				if str(a["itemType"]) != "annotation" {
					continue
				}
				annotations = append(annotations, "### Annotation "+str(d["key"])+" · p. "+SafeText(a["annotationPageLabel"])+"\n\n> "+SafeText(a["annotationText"])+"\n\n"+SafeText(a["annotationComment"]))
			}
		}
	}
	collections := []any{}
	for _, v := range array(data["collections"]) {
		c, e := s.ZT(ctx, "/collections/"+str(v), nil)
		if e != nil {
			return nil, e
		}
		collections = append(collections, Object{"key": v, "name": obj(obj(c)["data"])["name"]})
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	citekey, e := s.Citekey(ctx, data)
	if e != nil {
		return nil, e
	}
	authors, year := Authors(data), Year(data)
	tags := []any{}
	for _, v := range array(data["tags"]) {
		tags = append(tags, obj(v)["tag"])
	}
	meta := Object{"type": "literature", "zoteroLibrary": fmt.Sprintf("user:%d", user), "zoteroKey": key, "zoteroVersion": item["version"], "citekey": citekey, "title": str(data["title"]), "authors": authors, "year": year, "doi": str(data["DOI"]), "url": str(data["url"]), "abstract": str(data["abstractNote"]), "tags": tags, "collections": collections, "abstractLanguage": str(data["language"])}
	abstract := str(data["abstractNote"])
	if m := regexp.MustCompile(`(?mi)^Abstract\[pt(?:-BR)?\]:\s*(.+)$`).FindStringSubmatch(str(data["extra"])); m != nil {
		abstract = m[1]
		meta["abstractLanguage"] = "pt-BR"
	}
	reference := APAMarkdown(str(item["bib"]))
	if reference == "" {
		return nil, Conflict("Zotero did not return an APA bibliography")
	}
	tagText := []string{}
	for _, v := range tags {
		tagText = append(tagText, SafeText(v))
	}
	block := map[string]string{"REFERENCE": reference, "TAGS": fallback(strings.Join(tagText, ", "), "Sem tags no Zotero."), "ABSTRACT": fallback(SafeText(abstract), "Sem abstract disponível no Zotero."), "NOTES": fallback(strings.Join(append(notes, annotations...), "\n\n"), "Sem notas ou annotations no Zotero.")}
	author := "Sem autor"
	if len(authors) > 0 {
		author = authors[0]
	}
	yr := str(year)
	if yr == "" {
		yr = "s.d."
	}
	path, e := NotePath("Literature", author+" "+yr+" - "+str(data["title"]), key)
	if e != nil {
		return nil, e
	}
	return s.UpsertNote(ctx, "ZOTERO", key, path, meta, block)
}
func fallback(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func (s *Service) SyncZotero(ctx context.Context) error {
	checkpoint, e := s.State.Get("zotero_version", 0)
	if e != nil {
		return e
	}
	r, e := s.ztResponse(ctx, "/items", url.Values{"since": {str(checkpoint)}, "format": {"versions"}, "includeTrashed": {"1"}})
	if e != nil {
		return e
	}
	version, e := strconv.Atoi(r.Header.Get("Last-Modified-Version"))
	if e != nil {
		r.Body.Close()
		return e
	}
	v, e := decodeResponse(r, nil)
	if e != nil {
		return e
	}
	changed := obj(v)
	known, e := s.State.Identities("ZOTERO")
	if e != nil {
		return e
	}
	parents := map[string]bool{}
	if len(known) > 0 {
		for key := range changed {
			v, e := s.ZT(ctx, "/items/"+key, nil)
			if e != nil {
				return e
			}
			item := obj(obj(v)["data"])
			parent := str(item["parentItem"])
			if parent != "" {
				v, e = s.ZT(ctx, "/items/"+parent, nil)
				if e != nil {
					return e
				}
				parent = fallback(str(obj(obj(v)["data"])["parentItem"]), parent)
			}
			if hasString(known, key) {
				parents[key] = true
			} else if hasString(known, parent) {
				parents[parent] = true
			}
		}
	}
	for key := range parents {
		if _, e = s.ImportZotero(ctx, key); e != nil {
			return e
		}
	}
	registry, _, e := s.Read(ctx, "Bibliography/citekeys.json")
	if e != nil {
		return e
	}
	reg := Object{}
	if registry != nil {
		if e = json.Unmarshal([]byte(*registry), &reg); e != nil {
			return e
		}
	}
	for identity := range reg {
		parts := strings.Split(identity, ":")
		key := parts[len(parts)-1]
		if _, ok := changed[key]; ok {
			if _, e = s.Citation(ctx, key); e != nil {
				return e
			}
		}
	}
	r, e = s.ztResponse(ctx, "/deleted", url.Values{"since": {str(checkpoint)}})
	if e != nil {
		return e
	}
	deletedVersion, e := strconv.Atoi(r.Header.Get("Last-Modified-Version"))
	if e != nil {
		r.Body.Close()
		return e
	}
	if deletedVersion != version {
		r.Body.Close()
		return Conflict("Zotero changed during sync; checkpoint retained")
	}
	v, e = decodeResponse(r, nil)
	if e != nil {
		return e
	}
	for _, v := range array(obj(v)["items"]) {
		if hasString(known, str(v)) {
			if e = s.State.Set("zotero_removed:"+str(v), now()); e != nil {
				return e
			}
		}
	}
	return s.State.Set("zotero_version", version)
}
func hasString(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func (s *Service) RD(ctx context.Context, method, path string, params url.Values, body any, retries bool) (any, error) {
	key, e := s.Secret("readeck_token")
	if e != nil {
		return nil, e
	}
	return s.requestJSON(ctx, method, str(obj(s.Config["readeck"])["url"])+path, http.Header{"Authorization": {"Bearer " + key}}, params, body, retries)
}
func (s *Service) ReadeckSearch(ctx context.Context, q string) (any, error) {
	term := strings.TrimSpace(q)
	if regexp.MustCompile(`^[\pL\pN_\s]+$`).MatchString(term) {
		words := strings.Fields(term)
		for i := range words {
			words[i] += "*"
		}
		term = strings.Join(words, " ")
	}
	v, e := s.SearchData(ctx, "readeck", q, func() (any, error) {
		return s.RD(ctx, "GET", "/bookmarks", url.Values{"search": {term}, "limit": {"50"}}, nil, true)
	})
	if e != nil {
		return nil, e
	}
	out := []any{}
	for _, v := range array(v) {
		x := obj(v)
		note, e := s.State.Note("READECK", str(x["id"]))
		if e != nil {
			return nil, e
		}
		out = append(out, Object{"id": x["id"], "title": str(x["title"]), "url": str(x["url"]), "path": note["path"]})
	}
	return out, nil
}
func (s *Service) ImportReadeck(ctx context.Context, bookmark Object) (Object, error) {
	id := str(bookmark["id"])
	v, e := s.RD(ctx, "GET", "/bookmarks/"+id+"/annotations", nil, nil, true)
	if e != nil {
		return nil, e
	}
	parts := []string{}
	for _, v := range array(v) {
		a := obj(v)
		text := a["text"]
		if text == nil {
			text = a["content"]
		}
		note := a["note"]
		if note == nil {
			note = a["comment"]
		}
		parts = append(parts, "### Highlight "+str(a["id"])+"\n\n> "+HTMLText(str(text))+"\n\n"+HTMLText(str(note)))
	}
	block := strings.Join(parts, "\n\n")
	if str(bookmark["note"]) != "" {
		block = "### Nota do bookmark\n\n" + HTMLText(str(bookmark["note"])) + "\n\n" + block
	}
	meta := Object{"type": "reading", "readeckId": id, "title": str(bookmark["title"]), "url": str(bookmark["url"]), "authors": array(bookmark["authors"]), "created": bookmark["created"], "published": bookmark["published"], "labels": array(bookmark["labels"])}
	path, e := NotePath("Reading", str(bookmark["title"]), id)
	if e != nil {
		return nil, e
	}
	s.Lock.Lock()
	result, e := s.UpsertNote(ctx, "READECK", id, path, meta, block)
	s.Lock.Unlock()
	if e != nil {
		return nil, e
	}
	cfg := obj(s.Config["readeck"])
	triggers := array(cfg["trigger_labels"])
	triggered := false
	for _, label := range array(bookmark["labels"]) {
		if contains(triggers, label) {
			triggered = true
		}
	}
	if triggered {
		latest, e := s.RD(ctx, "GET", "/bookmarks/"+id, nil, nil, true)
		if e != nil {
			return nil, e
		}
		wanted := []any{}
		for _, label := range array(obj(latest)["labels"]) {
			if !contains(triggers, label) {
				wanted = append(wanted, label)
			}
		}
		processed := cfg["processed_label"]
		if !contains(wanted, processed) {
			wanted = append(wanted, processed)
		}
		if _, e = s.RD(ctx, "PATCH", "/bookmarks/"+id, nil, Object{"labels": wanted}, false); e != nil {
			return nil, e
		}
	}
	return result, nil
}
func (s *Service) ImportReadeckID(ctx context.Context, id string) (Object, error) {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`).MatchString(id) {
		return nil, invalid("Invalid Readeck bookmark ID")
	}
	v, e := s.RD(ctx, "GET", "/bookmarks/"+id, nil, nil, true)
	if e != nil {
		return nil, e
	}
	return s.ImportReadeck(ctx, obj(v))
}
func (s *Service) SyncReadeck(ctx context.Context) error {
	cursor, e := s.State.Get("readeck_cursor", nil)
	if e != nil {
		return e
	}
	started := time.Now().UTC()
	params := url.Values{}
	if str(cursor) != "" {
		params.Set("since", str(cursor))
	}
	v, e := s.RD(ctx, "GET", "/bookmarks/sync", params, nil, true)
	if e != nil {
		return e
	}
	updates := array(v)
	known, e := s.State.Identities("READECK")
	if e != nil {
		return e
	}
	ids := map[string]bool{}
	for _, v := range updates {
		x := obj(v)
		if str(x["type"]) != "delete" {
			ids[str(x["id"])] = true
		}
	}
	day := time.Now().Format("2006-01-02")
	full, e := s.State.Get("readeck_full_day", nil)
	if e != nil {
		return e
	}
	if full != day {
		for _, id := range known {
			ids[id] = true
		}
	}
	for id := range ids {
		v, e := s.RD(ctx, "GET", "/bookmarks/"+id, nil, nil, true)
		if e != nil {
			var up *UpstreamError
			if errors.As(e, &up) && up.Status == 404 {
				if e = s.State.Set("readeck_removed:"+id, now()); e != nil {
					return e
				}
				continue
			}
			return e
		}
		bookmark := obj(v)
		importIt := hasString(known, id)
		for _, label := range array(bookmark["labels"]) {
			if contains(array(obj(s.Config["readeck"])["trigger_labels"]), label) {
				importIt = true
			}
		}
		if importIt {
			if _, e = s.ImportReadeck(ctx, bookmark); e != nil {
				return e
			}
		}
	}
	for _, v := range updates {
		x := obj(v)
		if str(x["type"]) == "delete" && hasString(known, str(x["id"])) {
			if e = s.State.Set("readeck_removed:"+str(x["id"]), now()); e != nil {
				return e
			}
		}
	}
	if e = s.State.Set("readeck_cursor", started.Add(-2*time.Second).Format("2006-01-02T15:04:05.000000+00:00")); e != nil {
		return e
	}
	return s.State.Set("readeck_full_day", day)
}
