package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Service struct {
	Config                  Object
	State                   *State
	Client                  *http.Client
	Lock                    sync.Mutex
	SettingsDir, SecretsDir string
	Zone                    *time.Location
	Calendar                *GoogleCalendar
	cacheMu                 sync.Mutex
	searchLocks             map[string]*sync.Mutex
	searchCache             map[string]cacheEntry
	filterCache             *cacheEntry
	zoteroValidated         string
}
type cacheEntry struct {
	At    time.Time
	Value any
}

func NewService(config Object, state *State, client *http.Client) (*Service, error) {
	zone, e := time.LoadLocation(str(config["timezone"]))
	if e != nil {
		return nil, e
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second}}
	}
	s := &Service{Config: config, State: state, Client: client, SettingsDir: env("HUB_SETTINGS_DIR", "/state/configuration"), SecretsDir: env("HUB_SECRETS", "/run/secrets"), Zone: zone, searchLocks: map[string]*sync.Mutex{"zotero": {}, "readeck": {}}, searchCache: map[string]cacheEntry{}}
	s.Calendar = NewGoogleCalendar(s)
	return s, nil
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func (s *Service) Secret(name string) (string, error) {
	path := filepath.Join(s.SettingsDir, "secrets", name)
	if _, e := os.Stat(path); os.IsNotExist(e) {
		path = filepath.Join(s.SecretsDir, name)
	}
	b, e := os.ReadFile(path)
	if e != nil || strings.TrimSpace(string(b)) == "" {
		return "", NotConfigured("Configure secret file: " + name)
	}
	return strings.TrimSpace(string(b)), nil
}

type UpstreamError struct{ Status int }

func (e *UpstreamError) Error() string { return fmt.Sprintf("Upstream status %d", e.Status) }
func wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Request retries reads only when requested. A failed unrepeatable write is uncertain.
func (s *Service) Request(ctx context.Context, method, target string, headers http.Header, body []byte, retries bool) (*http.Response, error) {
	for attempt := 0; attempt < 4; attempt++ {
		req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
		if e != nil {
			return nil, e
		}
		req.Header = headers.Clone()
		r, e := s.Client.Do(req)
		if e != nil {
			if !retries {
				return nil, Uncertain("Remote write outcome unknown")
			}
			if attempt == 3 {
				return nil, &Error{502, "Upstream unavailable"}
			}
			if e = wait(ctx, time.Duration(1<<attempt)*time.Second); e != nil {
				return nil, e
			}
			continue
		}
		if r.StatusCode == 412 {
			r.Body.Close()
			return nil, Conflict("File changed while synchronizing; retry after rereading")
		}
		if retries && (r.StatusCode == 429 || r.StatusCode == 502 || r.StatusCode == 503 || r.StatusCode == 504) && attempt < 3 {
			r.Body.Close()
			d := time.Duration(1<<attempt) * time.Second
			v := r.Header.Get("Retry-After")
			if v == "" {
				v = r.Header.Get("Backoff")
			}
			if n, e := strconv.Atoi(v); e == nil && n >= 0 {
				d = time.Duration(n) * time.Second
			}
			if e = wait(ctx, d); e != nil {
				return nil, e
			}
			continue
		}
		if r.StatusCode >= 300 && r.StatusCode != 304 {
			r.Body.Close()
			return nil, &UpstreamError{r.StatusCode}
		}
		if n, e := strconv.Atoi(r.Header.Get("Backoff")); e == nil && n > 0 {
			if e = wait(ctx, time.Duration(n)*time.Second); e != nil {
				r.Body.Close()
				return nil, e
			}
		}
		return r, nil
	}
	return nil, errors.New("Request retry exhausted")
}
func decodeResponse(r *http.Response, e error) (any, error) {
	if e != nil {
		return nil, e
	}
	defer r.Body.Close()
	var v any
	e = json.NewDecoder(r.Body).Decode(&v)
	return v, e
}
func (s *Service) requestJSON(ctx context.Context, method, target string, headers http.Header, params url.Values, body any, retries bool) (any, error) {
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var b []byte
	var e error
	if body != nil {
		b, e = json.Marshal(body)
		if e != nil {
			return nil, e
		}
		headers = headers.Clone()
		if headers == nil {
			headers = http.Header{}
		}
		headers.Set("Content-Type", "application/json")
	}
	return decodeResponse(s.Request(ctx, method, target, headers, b, retries))
}
func (s *Service) sbHeaders() (http.Header, error) {
	v, e := s.Secret("silverbullet_token")
	return http.Header{"Authorization": {"Bearer " + v}, "X-Sync-Mode": {"true"}}, e
}
func (s *Service) Files(ctx context.Context) ([]string, error) {
	h, e := s.sbHeaders()
	if e != nil {
		return nil, e
	}
	v, e := s.requestJSON(ctx, "GET", str(s.Config["silverbullet_url"])+"/.fs", h, nil, nil, true)
	if e != nil {
		return nil, e
	}
	out := []string{}
	switch data := v.(type) {
	case map[string]any:
		for k := range data {
			out = append(out, k)
		}
	case []any:
		for _, v := range data {
			if x, ok := v.(string); ok {
				out = append(out, x)
			} else {
				x := obj(v)
				name := str(x["name"])
				if name == "" {
					name = str(x["path"])
				}
				out = append(out, name)
			}
		}
	}
	return out, nil
}
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func (s *Service) readRaw(ctx context.Context, path string) ([]byte, string, error) {
	if _, e := SafePath(path); e != nil {
		return nil, "", e
	}
	h, e := s.sbHeaders()
	if e != nil {
		return nil, "", e
	}
	r, e := s.Request(ctx, "GET", str(s.Config["silverbullet_url"])+"/.fs/"+escapePath(path), h, nil, true)
	if e != nil {
		var up *UpstreamError
		if errors.As(e, &up) && up.Status == 404 {
			return nil, "", nil
		}
		return nil, "", e
	}
	defer r.Body.Close()
	b, e := io.ReadAll(r.Body)
	return b, r.Header.Get("ETag"), e
}
func (s *Service) Read(ctx context.Context, path string) (*string, string, error) {
	b, tag, e := s.readRaw(ctx, path)
	if e != nil || b == nil {
		return nil, tag, e
	}
	v := string(b)
	return &v, tag, nil
}
func (s *Service) Bytes(ctx context.Context, path string) ([]byte, error) {
	b, _, e := s.readRaw(ctx, path)
	return b, e
}
func (s *Service) writeRaw(ctx context.Context, path string, data []byte, etag string, createOnly bool, contentType string) error {
	if _, e := SafePath(path); e != nil {
		return e
	}
	h, e := s.sbHeaders()
	if e != nil {
		return e
	}
	h.Set("Content-Type", contentType)
	if etag != "" {
		h.Set("If-Match", etag)
	} else if createOnly {
		h.Set("If-None-Match", "*")
	}
	r, e := s.Request(ctx, "PUT", str(s.Config["silverbullet_url"])+"/.fs/"+escapePath(path), h, data, false)
	if e == nil {
		r.Body.Close()
	}
	return e
}
func (s *Service) Write(ctx context.Context, path, text, etag string) error {
	return s.writeRaw(ctx, path, []byte(text), etag, true, "text/markdown; charset=utf-8")
}
func (s *Service) Put(ctx context.Context, path string, data []byte, createOnly bool) error {
	return s.writeRaw(ctx, path, data, "", createOnly, "application/octet-stream")
}
func (s *Service) VK(ctx context.Context, method, path string, params url.Values, body any, retries bool) (any, error) {
	key, e := s.Secret("vikunja_token")
	if e != nil {
		return nil, e
	}
	return s.requestJSON(ctx, method, str(obj(s.Config["vikunja"])["url"])+path, http.Header{"Authorization": {"Bearer " + key}}, params, body, retries)
}
func (s *Service) vkResponse(ctx context.Context, path string, params url.Values) (*http.Response, error) {
	key, e := s.Secret("vikunja_token")
	if e != nil {
		return nil, e
	}
	target := str(obj(s.Config["vikunja"])["url"]) + path
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	return s.Request(ctx, "GET", target, http.Header{"Authorization": {"Bearer " + key}}, nil, true)
}
func (s *Service) PaginatedVK(ctx context.Context, path string, params url.Values) ([]any, error) {
	out := []any{}
	for page := 1; page <= 10000; page++ {
		p := url.Values{}
		for k, v := range params {
			p[k] = v
		}
		p.Set("page", strconv.Itoa(page))
		p.Set("per_page", "100")
		r, e := s.vkResponse(ctx, path, p)
		if e != nil {
			return nil, e
		}
		pages, _ := strconv.Atoi(r.Header.Get("X-Pagination-Total-Pages"))
		v, e := decodeResponse(r, nil)
		if e != nil {
			return nil, e
		}
		data, ok := v.([]any)
		if !ok && v != nil {
			return nil, invalid("Unsupported Vikunja collection")
		}
		out = append(out, data...)
		if pages == 0 || page >= pages {
			return out, nil
		}
	}
	return nil, invalid("Vikunja pagination safety limit")
}
func (s *Service) UpdateTask(ctx context.Context, id int, patch Object) (Object, error) {
	v, e := s.VK(ctx, "GET", fmt.Sprintf("/tasks/%d", id), nil, nil, true)
	if e != nil {
		return nil, e
	}
	current := obj(v)
	task := clone(current)
	for k, v := range patch {
		if k != "labels" {
			task[k] = v
		}
	}
	if v, ok := patch["due_date"]; ok && regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`).MatchString(str(v)) {
		d, e := time.ParseInLocation("2006-01-02", str(v), s.Zone)
		if e != nil {
			return nil, invalid("Invalid due date")
		}
		task["due_date"] = d.Format(time.RFC3339)
	}
	if _, e = s.VK(ctx, "POST", fmt.Sprintf("/tasks/%d", id), nil, task, false); e != nil {
		return nil, e
	}
	if labels, ok := patch["labels"]; ok {
		wanted := array(labels)
		existing := []any{}
		for _, v := range array(current["labels"]) {
			existing = append(existing, obj(v)["id"])
		}
		for _, v := range wanted {
			if !contains(existing, v) {
				if _, e = s.VK(ctx, "PUT", fmt.Sprintf("/tasks/%d/labels", id), nil, Object{"label_id": v}, false); e != nil {
					return nil, e
				}
			}
		}
		for _, v := range existing {
			if !contains(wanted, v) {
				if _, e = s.VK(ctx, "DELETE", fmt.Sprintf("/tasks/%d/labels/%d", id, num(v)), nil, nil, false); e != nil {
					return nil, e
				}
			}
		}
	}
	s.cacheMu.Lock()
	s.filterCache = nil
	s.cacheMu.Unlock()
	v, e = s.VK(ctx, "GET", fmt.Sprintf("/tasks/%d", id), nil, nil, true)
	return obj(v), e
}
func (s *Service) FindOrigin(ctx context.Context, origin string) (Object, error) {
	tasks, e := s.PaginatedVK(ctx, "/tasks", url.Values{"s": {origin}})
	if e != nil {
		return nil, e
	}
	var match Object
	for _, v := range tasks {
		t := obj(v)
		if strings.Contains(str(t["description"]), "SilverBullet origin: "+origin) {
			if match != nil {
				return nil, Conflict("Duplicate origin found in Vikunja")
			}
			match = t
		}
	}
	return match, nil
}
func (s *Service) ReconcileDaily(ctx context.Context, path string) (Object, error) {
	if !DailyPattern.MatchString(path) {
		return nil, invalid("Only Daily/YYYY-MM-DD.md is eligible")
	}
	s.Lock.Lock()
	defer s.Lock.Unlock()
	text, etag, e := s.Read(ctx, path)
	if e != nil {
		return nil, e
	}
	if text == nil {
		return Object{"processed": 0}, nil
	}
	parsed, e := TaskLines(*text)
	if e != nil {
		return nil, e
	}
	lines := strings.Split(strings.TrimSuffix(*text, "\n"), "\n")
	for _, line := range parsed {
		lines[line.Index] = line.Render()
	}
	join := func(lines []string, original string) string {
		v := strings.Join(lines, "\n")
		if strings.HasSuffix(original, "\n") {
			v += "\n"
		}
		return v
	}
	if updated := join(lines, *text); updated != *text {
		if e = s.Write(ctx, path, updated, etag); e != nil {
			return nil, e
		}
	}
	processed := 0
	for _, line := range parsed {
		origin := str(line.Meta["origin"])
		record, e := s.State.Task(origin)
		if e != nil {
			return nil, e
		}
		id := num(line.Meta["id"])
		if id == 0 {
			id = num(record["id"])
		}
		base := obj(record["base"])
		var remote Object
		if id != 0 {
			v, e := s.VK(ctx, "GET", fmt.Sprintf("/tasks/%d", id), nil, nil, true)
			if e != nil {
				return nil, e
			}
			remote = obj(v)
			rp := TaskProperties(remote)
			merged := rp
			if len(base) > 0 {
				merged, e = MergeProperties(base, line.Properties(num(obj(s.Config["vikunja"])["inbox_id"])), rp)
				if e != nil {
					return nil, e
				}
			}
			if !equal(merged, rp) {
				remote, e = s.UpdateTask(ctx, id, merged)
				if e != nil {
					return nil, e
				}
			}
		} else {
			if str(record["phase"]) == "uncertain" {
				remote, e = s.FindOrigin(ctx, origin)
				if e != nil {
					return nil, e
				}
				if remote == nil {
					return nil, Uncertain("Create may have succeeded; reconcile before retrying")
				}
			} else {
				project := num(line.Properties(num(obj(s.Config["vikunja"])["inbox_id"]))["project_id"])
				if _, e = s.VK(ctx, "GET", fmt.Sprintf("/projects/%d", project), nil, nil, true); e != nil {
					return nil, e
				}
				if e = s.State.SaveTask(origin, nil, "uncertain", nil, path, nil); e != nil {
					return nil, e
				}
				p := line.Properties(project)
				body := Object{"title": line.Title, "done": line.Done, "description": "<p>SilverBullet origin: " + origin + "</p>", "due_date": p["due_date"], "priority": p["priority"]}
				v, e := s.VK(ctx, "PUT", fmt.Sprintf("/projects/%d/tasks", project), nil, body, false)
				if e != nil {
					var up *UpstreamError
					if errors.As(e, &up) && up.Status >= 400 && up.Status < 500 {
						if se := s.State.SaveTask(origin, nil, "rejected", nil, path, "Request rejected"); se != nil {
							return nil, se
						}
					}
					return nil, e
				}
				remote = obj(v)
				if len(array(line.Meta["labels"])) > 0 {
					remote, e = s.UpdateTask(ctx, num(remote["id"]), Object{"labels": line.Meta["labels"]})
					if e != nil {
						return nil, e
					}
				}
			}
			id = num(remote["id"])
		}
		props := TaskProperties(remote)
		if e = s.State.SaveTask(origin, id, "synced", props, path, nil); e != nil {
			return nil, e
		}
		latest, tag, e := s.Read(ctx, path)
		if e != nil {
			return nil, e
		}
		if latest == nil {
			return nil, Conflict("Daily disappeared during synchronization; remote ID is retained in state")
		}
		updated, e := TaskLines(*latest)
		if e != nil {
			return nil, e
		}
		out := strings.Split(strings.TrimSuffix(*latest, "\n"), "\n")
		for _, target := range updated {
			if str(target.Meta["origin"]) != origin {
				continue
			}
			if target.Title != line.Title || target.Done != line.Done || !equal(target.Meta, line.Meta) {
				return nil, Conflict("Daily changed during synchronization; remote ID is retained in state")
			}
			target.Meta["id"] = id
			target.Update(props)
			out[target.Index] = target.Render()
		}
		if text := join(out, *latest); text != *latest {
			if e = s.Write(ctx, path, text, tag); e != nil {
				return nil, e
			}
		}
		processed++
	}
	return Object{"processed": processed}, nil
}
func (s *Service) OpenDaily(ctx context.Context) (Object, error) {
	day := time.Now().In(s.Zone).Format("2006-01-02")
	path := "Daily/" + day + ".md"
	v, _, e := s.Read(ctx, path)
	if e != nil {
		return nil, e
	}
	if v == nil {
		e = s.Write(ctx, path, "---\ntype: daily\ndate: "+day+"\n---\n\n# "+day+"\n\n## Diário\n\n", "")
		var conflict *Error
		if errors.As(e, &conflict) && conflict.Status == 409 {
			v, _, re := s.Read(ctx, path)
			if re != nil {
				return nil, re
			}
			if v != nil {
				e = nil
			}
		}
		if e != nil {
			return nil, e
		}
	}
	return Object{"path": strings.TrimSuffix(path, ".md")}, nil
}
func (s *Service) SavedFilter(ctx context.Context, force bool) (any, error) {
	s.cacheMu.Lock()
	cached := s.filterCache
	s.cacheMu.Unlock()
	if !force && cached != nil && time.Since(cached.At) < 30*time.Second {
		return cached.Value, nil
	}
	v, e := s.VK(ctx, "GET", fmt.Sprintf("/filters/%d", num(obj(s.Config["vikunja"])["saved_filter_id"])), nil, nil, true)
	if e != nil {
		return nil, e
	}
	f := obj(v)
	params := url.Values{}
	for _, k := range []string{"filter", "filter_timezone", "filter_include_nulls", "sort_by", "order_by", "s"} {
		if v := obj(f["filters"])[k]; v != nil {
			if arr, ok := v.([]any); ok {
				for _, v := range arr {
					params.Add(k, str(v))
				}
			} else {
				params.Set(k, str(v))
			}
		}
	}
	if params.Get("filter_timezone") == "" {
		params.Set("filter_timezone", str(s.Config["timezone"]))
	}
	tasks, e := s.PaginatedVK(ctx, "/tasks", params)
	if e != nil {
		return nil, e
	}
	data := Object{"title": f["title"], "updated": now(), "tasks": tasks}
	s.cacheMu.Lock()
	s.filterCache = &cacheEntry{time.Now(), data}
	s.cacheMu.Unlock()
	return data, nil
}
func (s *Service) TasksToday(ctx context.Context) (Object, error) {
	d := time.Now().In(s.Zone)
	start := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, s.Zone)
	tasks, e := s.PaginatedVK(ctx, "/tasks", url.Values{"filter": {fmt.Sprintf(`done = false && due_date >= "%s" && due_date < "%s"`, start.Format(time.RFC3339), start.AddDate(0, 0, 1).Format(time.RFC3339))}, "filter_timezone": {str(s.Config["timezone"])}})
	return Object{"updated": now(), "tasks": tasks}, e
}
func now() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }
func (s *Service) SearchData(ctx context.Context, source, term string, fetch func() (any, error)) (any, error) {
	key := source + ":" + strings.ToLower(strings.TrimSpace(term))
	lock, ok := s.searchLocks[source]
	if !ok {
		return nil, invalid("Unknown search source")
	}
	lock.Lock()
	defer lock.Unlock()
	s.cacheMu.Lock()
	c, ok := s.searchCache[key]
	s.cacheMu.Unlock()
	if ok && time.Since(c.At) < 60*time.Second {
		return c.Value, nil
	}
	v, e := fetch()
	if e != nil {
		return nil, e
	}
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if len(s.searchCache) >= 64 {
		var oldest string
		var stamp time.Time
		for k, v := range s.searchCache {
			if oldest == "" || v.At.Before(stamp) {
				oldest, stamp = k, v.At
			}
		}
		delete(s.searchCache, oldest)
	}
	s.searchCache[key] = cacheEntry{time.Now(), v}
	return v, nil
}
