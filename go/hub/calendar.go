package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const CalendarAPI = "https://www.googleapis.com/calendar/v3"

type GoogleCalendar struct {
	Service *Service
	Folder  string
	Lock    sync.Mutex
	access  string
	expires time.Time
}

func NewGoogleCalendar(s *Service) *GoogleCalendar {
	return &GoogleCalendar{Service: s, Folder: fallback(str(obj(s.Config["google_calendar"])["directory"]), "/calendar")}
}
func readObject(path string) (Object, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	v := Object{}
	e = json.Unmarshal(b, &v)
	return v, e
}
func (c *GoogleCalendar) Connection() (Object, error) {
	v, e := readObject(filepath.Join(c.Folder, "calendar.json"))
	if e != nil {
		return nil, NotConfigured("Google Calendar: autorização/configuração pendente")
	}
	if str(v["calendar_id"]) == "" || str(v["instance_id"]) == "" || v["calendar_id"] == "primary" {
		return nil, NotConfigured("Google Calendar: calendário dedicado não configurado")
	}
	return v, nil
}
func (c *GoogleCalendar) headers(ctx context.Context) (http.Header, error) {
	if c.access == "" || time.Until(c.expires) < 60*time.Second {
		credentials, e := readObject(filepath.Join(c.Folder, "oauth.json"))
		if e != nil {
			return nil, NotConfigured("Google Calendar: autorização OAuth pendente")
		}
		data := url.Values{"grant_type": {"refresh_token"}}
		for _, k := range []string{"client_id", "client_secret", "refresh_token"} {
			if str(credentials[k]) == "" {
				return nil, NotConfigured("Google Calendar: autorização OAuth pendente")
			}
			data.Set(k, str(credentials[k]))
		}
		r, e := c.Service.Request(ctx, "POST", "https://oauth2.googleapis.com/token", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, []byte(data.Encode()), true)
		if e != nil {
			var up *UpstreamError
			if errors.As(e, &up) && (up.Status == 400 || up.Status == 401) {
				return nil, NotConfigured("Google Calendar: reautorize a conta Google")
			}
			return nil, e
		}
		v, e := decodeResponse(r, nil)
		if e != nil {
			return nil, e
		}
		value := obj(v)
		c.access = str(value["access_token"])
		if c.access == "" {
			return nil, NotConfigured("Google Calendar: autorização OAuth pendente")
		}
		expires := num(value["expires_in"])
		if expires == 0 {
			expires = 3600
		}
		c.expires = time.Now().Add(time.Duration(expires) * time.Second)
		if str(value["refresh_token"]) != "" {
			credentials["refresh_token"] = value["refresh_token"]
			b, e := jsonPretty(credentials)
			if e != nil {
				return nil, e
			}
			if e = PrivateWrite(filepath.Join(c.Folder, "oauth.json"), []byte(b)); e != nil {
				return nil, e
			}
		}
	}
	return http.Header{"Authorization": {"Bearer " + c.access}}, nil
}
func (c *GoogleCalendar) google(ctx context.Context, method, path string, params url.Values, body any) (any, error) {
	for attempt := 0; attempt < 2; attempt++ {
		h, e := c.headers(ctx)
		if e != nil {
			return nil, e
		}
		v, e := c.Service.requestJSON(ctx, method, CalendarAPI+path, h, params, body, true)
		var up *UpstreamError
		if errors.As(e, &up) && up.Status == 401 && attempt == 0 {
			c.access = ""
			continue
		}
		return v, e
	}
	return nil, NotConfigured("Google Calendar: autorização OAuth pendente")
}
func parseDate(s string, zone *time.Location) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02"} {
		var t time.Time
		var e error
		if layout == time.RFC3339Nano {
			t, e = time.Parse(layout, s)
		} else {
			t, e = time.ParseInLocation(layout, s, zone)
		}
		if e == nil {
			return t, nil
		}
	}
	return time.Time{}, invalid("Unsupported Vikunja due date")
}
func (c *GoogleCalendar) Payload(task Object, instance string, projects map[int]string) (Object, error) {
	id := num(task["id"])
	_, titleOK := task["title"].(string)
	_, doneOK := task["done"].(bool)
	due, dueOK := task["due_date"]
	if id <= 0 || !equal(task["id"], id) || !titleOK || !doneOK || !dueOK {
		return nil, invalid("Unsupported Vikunja task response")
	}
	if due == nil || due == "" {
		return nil, nil
	}
	ds, ok := due.(string)
	if !ok {
		return nil, invalid("Unsupported Vikunja due date")
	}
	if strings.HasPrefix(ds, "0001-01-01") {
		return nil, nil
	}
	date, e := parseDate(ds, c.Service.Zone)
	if e != nil {
		return nil, e
	}
	date = date.In(c.Service.Zone)
	day := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, c.Service.Zone)
	generation, e := c.Service.State.Get(fmt.Sprintf("calendar:generation:%d", id), 0)
	if e != nil {
		return nil, e
	}
	identity := Digest(fmt.Sprintf("%s:%d:%d", instance, id, num(generation)))
	base := strings.TrimRight(str(obj(c.Service.Config["vikunja"])["public_url"]), "/")
	description := fmt.Sprintf("Vikunja · tarefa #%d\nProjeto: %s\n%s/tasks/%d\nEspelho automático. Edite a tarefa no Vikunja.", id, projects[num(task["project_id"])], base, id)
	summary := str(task["title"])
	if boolean(task["done"]) {
		summary = "✅ " + summary
	}
	return Object{"id": identity, "summary": summary, "description": description, "start": Object{"date": day.Format("2006-01-02")}, "end": Object{"date": day.AddDate(0, 0, 1).Format("2006-01-02")}, "status": "confirmed", "transparency": "transparent", "reminders": Object{"useDefault": false}, "extendedProperties": Object{"private": Object{"hubInstance": instance, "vikunjaTaskId": strconv.Itoa(id)}}}, nil
}
func (c *GoogleCalendar) Tasks(ctx context.Context) ([]any, error) {
	out := []any{}
	seen := map[int]bool{}
	for page := 1; page <= 10000; page++ {
		r, e := c.Service.vkResponse(ctx, "/tasks", url.Values{"page": {strconv.Itoa(page)}, "per_page": {"100"}, "sort_by": {"id"}, "order_by": {"asc"}})
		if e != nil {
			return nil, e
		}
		pages, pe := strconv.Atoi(r.Header.Get("X-Pagination-Total-Pages"))
		hasHeader := r.Header.Get("X-Pagination-Total-Pages") != ""
		v, e := decodeResponse(r, nil)
		if e != nil {
			return nil, e
		}
		if hasHeader && pe != nil {
			return nil, invalid("Unstable Vikunja pagination")
		}
		data, ok := v.([]any)
		if !ok {
			return nil, invalid("Unsupported Vikunja task collection")
		}
		for _, v := range data {
			task, ok := v.(map[string]any)
			id := num(task["id"])
			if !ok || id <= 0 || !equal(task["id"], id) || seen[id] {
				return nil, invalid("Unstable Vikunja pagination")
			}
			seen[id] = true
			out = append(out, task)
		}
		if len(data) == 0 || hasHeader && page >= pages {
			return out, nil
		}
	}
	return nil, invalid("Vikunja pagination safety limit")
}
func (c *GoogleCalendar) BumpGeneration(id string) error {
	key := "calendar:generation:" + id
	v, e := c.Service.State.Get(key, 0)
	if e != nil {
		return e
	}
	return c.Service.State.Set(key, num(v)+1)
}
func (c *GoogleCalendar) Sync(ctx context.Context) error {
	c.Lock.Lock()
	defer c.Lock.Unlock()
	connection, e := c.Connection()
	if e != nil {
		return e
	}
	instance := str(connection["instance_id"])
	base := "/calendars/" + url.PathEscape(str(connection["calendar_id"])) + "/events"
	tasks, e := c.Tasks(ctx)
	if e != nil {
		return e
	}
	rawProjects, e := c.Service.PaginatedVK(ctx, "/projects", nil)
	if e != nil {
		return e
	}
	projects := map[int]string{}
	for _, v := range rawProjects {
		p := obj(v)
		projects[num(p["id"])] = str(p["title"])
	}
	expected := map[string]Object{}
	expectedTasks := map[string]bool{}
	for _, v := range tasks {
		payload, e := c.Payload(obj(v), instance, projects)
		if e != nil {
			return e
		}
		if payload != nil {
			expected[str(payload["id"])] = payload
			expectedTasks[str(obj(obj(payload["extendedProperties"])["private"])["vikunjaTaskId"])] = true
		}
	}
	existing := map[string]Object{}
	token := ""
	seenTokens := map[string]bool{}
	for {
		params := url.Values{"privateExtendedProperty": {"hubInstance=" + instance}, "maxResults": {"2500"}}
		if token != "" {
			params.Set("pageToken", token)
		}
		v, e := c.google(ctx, "GET", base, params, nil)
		if e != nil {
			return e
		}
		data := obj(v)
		if _, ok := v.(map[string]any); !ok {
			return invalid("Unsupported Google Calendar response")
		}
		for _, v := range array(data["items"]) {
			event := obj(v)
			if str(event["status"]) != "cancelled" {
				existing[str(event["id"])] = event
			}
		}
		token = str(data["nextPageToken"])
		if token == "" {
			break
		}
		if seenTokens[token] {
			return invalid("Unstable Google Calendar pagination")
		}
		seenTokens[token] = true
	}
	changed, removed := 0, 0
	for id, payload := range expected {
		event, ok := existing[id]
		patch := clone(payload)
		delete(patch, "id")
		if !ok {
			_, e = c.google(ctx, "POST", base, nil, payload)
			var up *UpstreamError
			if errors.As(e, &up) && up.Status == 409 {
				v, re := c.google(ctx, "GET", base+"/"+id, nil, nil)
				if re != nil {
					var readUp *UpstreamError
					if !errors.As(re, &readUp) || (readUp.Status != 404 && readUp.Status != 410) {
						return re
					}
				} else {
					event = obj(v)
				}
				if re != nil || str(event["status"]) == "cancelled" {
					if e = c.BumpGeneration(str(obj(obj(payload["extendedProperties"])["private"])["vikunjaTaskId"])); e != nil {
						return e
					}
					return Conflict("Google Calendar: evento excluído será recriado no próximo ciclo")
				}
				if !equal(obj(event["extendedProperties"])["private"], obj(payload["extendedProperties"])["private"]) {
					return Conflict("Google Calendar: identidade de evento incompatível")
				}
				_, e = c.google(ctx, "PATCH", base+"/"+id, nil, patch)
			}
			if e != nil {
				return e
			}
			changed++
		} else {
			different := false
			for k, v := range patch {
				if !equal(event[k], v) {
					different = true
				}
			}
			if different {
				if _, e = c.google(ctx, "PATCH", base+"/"+id, nil, patch); e != nil {
					return e
				}
				changed++
			}
		}
	}
	for id, event := range existing {
		if _, ok := expected[id]; ok {
			continue
		}
		private := obj(obj(event["extendedProperties"])["private"])
		taskID := str(private["vikunjaTaskId"])
		if private["hubInstance"] != instance || !regexp.MustCompile(`^\d+$`).MatchString(taskID) {
			continue
		}
		v, e := c.Service.VK(ctx, "GET", "/tasks/"+taskID, nil, nil, true)
		if e != nil {
			var up *UpstreamError
			if !errors.As(e, &up) || up.Status != 404 {
				return e
			}
			old, e := c.Service.State.Get("calendar:task:"+taskID, Object{})
			if e != nil {
				return e
			}
			if _, ok := projects[num(obj(old)["project_id"])]; !ok {
				continue
			}
		} else {
			current, e := c.Payload(obj(v), instance, projects)
			if e != nil {
				return e
			}
			if current != nil && current["id"] == id {
				continue
			}
		}
		_, e = c.google(ctx, "DELETE", base+"/"+url.PathEscape(id), nil, nil)
		if e != nil {
			var up *UpstreamError
			if !errors.As(e, &up) || (up.Status != 404 && up.Status != 410) {
				return e
			}
		}
		if !expectedTasks[taskID] {
			if e = c.BumpGeneration(taskID); e != nil {
				return e
			}
		}
		removed++
	}
	for _, v := range tasks {
		task := obj(v)
		if e = c.Service.State.Set("calendar:task:"+str(task["id"]), Object{"project_id": task["project_id"]}); e != nil {
			return e
		}
	}
	return c.Service.State.Set("calendar:summary", Object{"tasks": len(expected), "changed": changed, "removed": removed})
}
func (c *GoogleCalendar) Run(ctx context.Context) error {
	e := c.Sync(ctx)
	if e != nil {
		v, se := c.Service.State.Get("calendar:failures", 0)
		if se != nil {
			return se
		}
		if se = c.Service.State.Set("calendar:failures", num(v)+1); se != nil {
			return se
		}
		return e
	}
	return c.Service.State.Set("calendar:failures", 0)
}
