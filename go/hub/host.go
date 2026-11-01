package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Job runs serially with other jobs, just like the Python worker. Register before Serve.
type Job struct {
	Name     string
	Interval time.Duration
	Run      func(context.Context) error
}

// Host is the public composition surface. Private modules register on Mux before Handler.
// Lock is Service.Lock: settings and task reconciliation share one process-wide lock.
type Host struct {
	Service *Service
	Mux     *http.ServeMux
	KV      *KVAdapter
	jobs    []Job
	jobMu   sync.Mutex
}

// KVAdapter exposes JSON state without changing Python's SQLite schema.
type KVAdapter struct{ State *State }

func (k *KVAdapter) Get(ctx context.Context, key string, out any) (bool, error) {
	if e := ctx.Err(); e != nil {
		return false, e
	}
	v, e := k.State.Get(key, nil)
	if e != nil {
		return false, e
	}
	if v == nil {
		var n int
		e = k.State.DB.QueryRowContext(ctx, "SELECT count(*) FROM kv WHERE key=?", key).Scan(&n)
		if e != nil || n == 0 {
			return false, e
		}
	}
	b, e := json.Marshal(v)
	if e != nil {
		return false, e
	}
	return true, json.Unmarshal(b, out)
}
func (k *KVAdapter) Set(ctx context.Context, key string, value any) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return k.State.Set(key, value)
}
func (k *KVAdapter) Keys(ctx context.Context, prefix string) ([]string, error) {
	rows, e := k.State.DB.QueryContext(ctx, "SELECT key FROM kv WHERE substr(key,1,?)=? ORDER BY key", len([]rune(prefix)), prefix)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var key string
		if e = rows.Scan(&key); e != nil {
			return nil, e
		}
		out = append(out, key)
	}
	return out, rows.Err()
}
func (k *KVAdapter) NoteHash(ctx context.Context, source, id string) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	v, e := k.State.Note(source, id)
	return str(v["hash"]), e
}
func (k *KVAdapter) SaveNote(ctx context.Context, source, id, path, block string) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	return k.State.SaveNote(source, id, path, block)
}
func NewHost(s *Service) *Host {
	h := &Host{Service: s, Mux: http.NewServeMux(), KV: &KVAdapter{s.State}}
	h.registerCore()
	return h
}
func (h *Host) RegisterRoute(pattern string, handler http.HandlerFunc) {
	h.Mux.HandleFunc(pattern, handler)
}
func (h *Host) RegisterJob(j Job) error {
	if j.Name == "" || j.Interval <= 0 || j.Run == nil {
		return errors.New("invalid extension job")
	}
	h.jobMu.Lock()
	defer h.jobMu.Unlock()
	for _, v := range h.jobs {
		if v.Name == j.Name {
			return fmt.Errorf("duplicate job: %s", j.Name)
		}
	}
	h.jobs = append(h.jobs, j)
	return nil
}
func (h *Host) Jobs() []Job {
	h.jobMu.Lock()
	defer h.jobMu.Unlock()
	return append([]Job{}, h.jobs...)
}

// Respond gives extensions the same sanitized error envelope as Relay routes.
func Respond(w http.ResponseWriter, value any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status, detail := 500, "Internal server error"
		var known *Error
		var up *UpstreamError
		if errors.As(err, &known) {
			status, detail = known.Status, known.Detail
		} else if errors.As(err, &up) {
			status, detail = 502, up.Error()
		} else if errors.Is(err, context.DeadlineExceeded) {
			status, detail = 502, "Upstream unavailable"
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(Object{"detail": detail})
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}
func (h *Host) route(pattern string, fn func(*http.Request) (any, error)) {
	h.Mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { v, e := fn(r); Respond(w, v, e) })
}
func decodeBody(r *http.Request, v any, strict bool) error {
	d := json.NewDecoder(r.Body)
	if strict {
		d.DisallowUnknownFields()
	}
	if e := d.Decode(v); e != nil {
		var max *http.MaxBytesError
		if errors.As(e, &max) {
			return &Error{413, "Request too large"}
		}
		return invalid("Invalid request body")
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		return invalid("Invalid request body")
	}
	return nil
}
func (h *Host) registerCore() {
	s := h.Service
	h.route("GET /health/live", func(r *http.Request) (any, error) { return Object{"ok": true}, nil })
	for _, path := range []string{"/health/ready", "/v1/status"} {
		h.route("GET "+path, func(r *http.Request) (any, error) { return h.Status() })
	}
	h.route("POST /v1/daily/reconcile", func(r *http.Request) (any, error) {
		var b struct {
			Path *string `json:"path"`
		}
		if e := decodeBody(r, &b, false); e != nil {
			return nil, e
		}
		if b.Path == nil || len([]rune(*b.Path)) > 200 {
			return nil, invalid("Invalid daily path")
		}
		return s.ReconcileDaily(r.Context(), *b.Path)
	})
	h.route("POST /v1/daily/open", func(r *http.Request) (any, error) { return s.OpenDaily(r.Context()) })
	h.route("GET /v1/tasks/saved-filter", func(r *http.Request) (any, error) {
		force := false
		if raw := r.URL.Query().Get("force"); raw != "" {
			var e error
			force, e = strconv.ParseBool(raw)
			if e != nil {
				return nil, invalid("Invalid force")
			}
		}
		return s.SavedFilter(r.Context(), force)
	})
	h.route("GET /v1/tasks/today", func(r *http.Request) (any, error) { return s.TasksToday(r.Context()) })
	h.route("GET /v1/tasks/{task_id}", func(r *http.Request) (any, error) {
		id, e := strconv.Atoi(r.PathValue("task_id"))
		if e != nil {
			return nil, invalid("Invalid task ID")
		}
		return s.VK(r.Context(), "GET", fmt.Sprintf("/tasks/%d", id), nil, nil, true)
	})
	h.route("PATCH /v1/tasks/{task_id}", func(r *http.Request) (any, error) {
		id, e := strconv.Atoi(r.PathValue("task_id"))
		if e != nil {
			return nil, invalid("Invalid task ID")
		}
		patch := Object{}
		if e = decodeBody(r, &patch, false); e != nil {
			return nil, e
		}
		clean := Object{}
		for k, v := range patch {
			if v == nil {
				continue
			}
			switch k {
			case "title":
				value, ok := v.(string)
				if !ok || len([]rune(value)) < 1 || len([]rune(value)) > 250 {
					return nil, invalid("Invalid title")
				}
			case "done":
				if _, ok := v.(bool); !ok {
					return nil, invalid("Invalid done")
				}
			case "due_date":
				value, ok := v.(string)
				if !ok || len([]rune(value)) > 40 {
					return nil, invalid("Invalid due date")
				}
			case "priority", "project_id":
				if !equal(v, num(v)) || k == "priority" && (num(v) < 0 || num(v) > 5) || k == "project_id" && num(v) <= 0 {
					return nil, invalid("Invalid task field")
				}
			case "labels":
				a, ok := v.([]any)
				if !ok || len(a) > 100 {
					return nil, invalid("Invalid labels")
				}
				for _, v := range a {
					if !equal(v, num(v)) {
						return nil, invalid("Invalid label ID")
					}
				}
			default:
				continue
			}
			clean[k] = v
		}
		s.Lock.Lock()
		defer s.Lock.Unlock()
		return s.UpdateTask(r.Context(), id, clean)
	})
	for _, name := range []string{"projects", "labels"} {
		name := name
		h.route("GET /v1/"+name, func(r *http.Request) (any, error) { return s.PaginatedVK(r.Context(), "/"+name, nil) })
	}
	h.route("GET /v1/zotero/identity", func(r *http.Request) (any, error) {
		id, e := s.ZoteroUser(r.Context())
		return Object{"user_id": id}, e
	})
	h.route("GET /v1/zotero/search", func(r *http.Request) (any, error) {
		q := r.URL.Query().Get("q")
		if len([]rune(q)) > 200 {
			return nil, invalid("Query too long")
		}
		return s.ZoteroSearch(r.Context(), q)
	})
	h.route("POST /v1/zotero/items/{key}/import", func(r *http.Request) (any, error) { return s.ImportZotero(r.Context(), r.PathValue("key")) })
	h.route("GET /v1/zotero/items/{key}/citation", func(r *http.Request) (any, error) { return s.Citation(r.Context(), r.PathValue("key")) })
	h.route("POST /v1/zotero/items/{key}/cite", func(r *http.Request) (any, error) { return s.CiteZotero(r.Context(), r.PathValue("key")) })
	h.route("GET /v1/readeck/search", func(r *http.Request) (any, error) {
		q := r.URL.Query().Get("q")
		if len([]rune(q)) > 200 {
			return nil, invalid("Query too long")
		}
		return s.ReadeckSearch(r.Context(), q)
	})
	h.route("POST /v1/readeck/bookmarks/{uid}/import", func(r *http.Request) (any, error) { return s.ImportReadeckID(r.Context(), r.PathValue("uid")) })
	h.route("GET /v1/settings", func(r *http.Request) (any, error) { return s.SettingsView() })
	h.route("PUT /v1/settings", func(r *http.Request) (any, error) {
		b := DefaultSettings()
		if e := decodeBody(r, &b, true); e != nil {
			return nil, e
		}
		return s.SaveSettings(r.Context(), b)
	})
	h.route("PUT /v1/settings/secrets/{name}", func(r *http.Request) (any, error) {
		var b struct {
			Value string `json:"value"`
		}
		if e := decodeBody(r, &b, true); e != nil {
			return nil, e
		}
		return s.SaveSecret(r.PathValue("name"), b.Value)
	})
	_ = h.RegisterJob(Job{"daily", time.Duration(defaultInt(s.Config["daily_interval"], 30)) * time.Second, func(ctx context.Context) error {
		if !enabled(s.Config, "daily") {
			return nil
		}
		files, e := s.Files(ctx)
		if e != nil {
			return e
		}
		for _, path := range files {
			if DailyPattern.MatchString(path) {
				if _, e = s.ReconcileDaily(ctx, path); e != nil {
					return e
				}
			}
		}
		return nil
	}})
	_ = h.RegisterJob(Job{"zotero", time.Duration(defaultInt(s.Config["zotero_interval"], 600)) * time.Second, func(ctx context.Context) error {
		if !enabled(s.Config, "zotero") {
			return nil
		}
		return s.SyncZotero(ctx)
	}})
	_ = h.RegisterJob(Job{"readeck", time.Duration(defaultInt(s.Config["readeck_interval"], 300)) * time.Second, func(ctx context.Context) error {
		if !enabled(s.Config, "readeck") {
			return nil
		}
		return s.SyncReadeck(ctx)
	}})
	_ = h.RegisterJob(Job{"google_calendar", time.Duration(max(60, defaultInt(obj(s.Config["google_calendar"])["interval"], 300))) * time.Second, func(ctx context.Context) error {
		if !boolean(obj(s.Config["google_calendar"])["enabled"]) {
			return nil
		}
		return s.Calendar.Run(ctx)
	}})
}

// Handler applies loopback and streaming request limits to core AND extension routes.
func (h *Host) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, e := net.SplitHostPort(r.RemoteAddr)
		if e != nil {
			host = r.RemoteAddr
		}
		if host != "127.0.0.1" && host != "::1" {
			Respond(w, nil, &Error{403, "Loopback only"})
			return
		}
		limit := int64(65536)
		if r.URL.Path == "/v1/paperwork/documents" {
			limit = 24100000
		} else if strings.HasSuffix(r.URL.Path, "/propose") {
			limit = 2100000
		}
		if raw := r.Header.Get("Content-Length"); raw != "" {
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil {
				Respond(w, nil, &Error{400, "Invalid Content-Length"})
				return
			}
			if n < 0 || n > limit {
				Respond(w, nil, &Error{413, "Request too large"})
				return
			}
		}
		if r.ContentLength > limit {
			Respond(w, nil, &Error{413, "Request too large"})
			return
		}
		if r.Body != nil {
			b, e := io.ReadAll(io.LimitReader(r.Body, limit+1))
			r.Body.Close()
			if e != nil {
				Respond(w, nil, &Error{400, "Invalid request body"})
				return
			}
			if int64(len(b)) > limit {
				Respond(w, nil, &Error{413, "Request too large"})
				return
			}
			r.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		h.Mux.ServeHTTP(w, r)
	})
}
func (s *Service) RunJob(ctx context.Context, name string, fn func(context.Context) error) error {
	e := fn(ctx)
	if errors.Is(e, context.Canceled) {
		return e
	}
	if e == nil {
		return s.State.Set("job:"+name, Object{"last_success": now(), "error": nil})
	}
	previous, se := s.State.Get("job:"+name, Object{})
	if se != nil {
		return se
	}
	message := "Error"
	var known *Error
	if errors.As(e, &known) {
		message = known.Detail
	} else {
		message = fmt.Sprintf("%T", e)
	}
	if se = s.State.Set("job:"+name, Object{"last_success": obj(previous)["last_success"], "error": message, "last_error": now()}); se != nil {
		return se
	}
	return e
}

// Worker is opt-in; constructing a host never starts remote jobs.
func (h *Host) Worker(ctx context.Context) error {
	last := map[string]time.Time{}
	for {
		for _, j := range h.Jobs() {
			if time.Since(last[j.Name]) >= j.Interval {
				_ = h.Service.RunJob(ctx, j.Name, j.Run)
				last[j.Name] = time.Now()
			}
			if e := ctx.Err(); e != nil {
				return e
			}
		}
		if e := wait(ctx, 5*time.Second); e != nil {
			return e
		}
	}
}
func (h *Host) Status() (Object, error) {
	jobs := Object{}
	for _, j := range h.Jobs() {
		v, e := h.Service.State.Get("job:"+j.Name, Object{})
		if e != nil {
			return nil, e
		}
		jobs[j.Name] = v
	}
	var count int
	if e := h.Service.State.DB.QueryRow("SELECT count(*) FROM tasks WHERE phase='uncertain'").Scan(&count); e != nil {
		return nil, e
	}
	c := h.Service.Calendar
	gc := Object{"enabled": boolean(obj(h.Service.Config["google_calendar"])["enabled"]), "authorized": exists(filepath.Join(c.Folder, "oauth.json")), "calendar_configured": exists(filepath.Join(c.Folder, "calendar.json"))}
	summary, e := h.Service.State.Get("calendar:summary", Object{})
	if e != nil {
		return nil, e
	}
	for k, v := range obj(summary) {
		gc[k] = v
	}
	git := Object{"error": "Backup status unavailable"}
	if exists("/backup-status/status.json") {
		var e error
		git, e = readObject("/backup-status/status.json")
		if e != nil {
			return nil, e
		}
	}
	return Object{"jobs": jobs, "google_calendar": gc, "uncertain_tasks": count, "git": git}, nil
}
func exists(path string) bool { _, e := os.Stat(path); return e == nil }
