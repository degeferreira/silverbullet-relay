package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PrivateWrite atomically replaces a file with mode 0600 and fsyncs its directory.
func PrivateWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, "."+filepath.Base(path)+"-")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func ValidateURL(value string) (string, error) {
	u, e := url.Parse(value)
	if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return "", invalid("Use an HTTP(S) server URL without credentials, query or fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

type Settings struct {
	Timezone               string `json:"timezone"`
	VikunjaURL             string `json:"vikunja_url"`
	VikunjaPublicURL       string `json:"vikunja_public_url"`
	InboxID                int    `json:"inbox_id"`
	SavedFilterID          int    `json:"saved_filter_id"`
	ReadeckURL             string `json:"readeck_url"`
	ZoteroUserID           *int   `json:"zotero_user_id"`
	DailyInterval          int    `json:"daily_interval"`
	ZoteroInterval         int    `json:"zotero_interval"`
	ReadeckInterval        int    `json:"readeck_interval"`
	ReadeckTriggerLabels   string `json:"readeck_trigger_labels"`
	ReadeckProcessedLabel  string `json:"readeck_processed_label"`
	GoogleCalendarEnabled  bool   `json:"google_calendar_enabled"`
	GoogleCalendarInterval int    `json:"google_calendar_interval"`
	DailyEnabled           bool   `json:"daily_enabled"`
	ZoteroEnabled          bool   `json:"zotero_enabled"`
	ReadeckEnabled         bool   `json:"readeck_enabled"`
}

func DefaultSettings() Settings {
	return Settings{DailyInterval: 30, ZoteroInterval: 600, ReadeckInterval: 300, ReadeckTriggerLabels: "silverbullet, process", ReadeckProcessedLabel: "silverbullet-imported", GoogleCalendarInterval: 300}
}
func (s *Service) SettingsView() (Object, error) {
	c := s.Config
	vk, rd, zt, gc := obj(c["vikunja"]), obj(c["readeck"]), obj(c["zotero"]), obj(c["google_calendar"])
	labels := []string{}
	for _, v := range array(rd["trigger_labels"]) {
		labels = append(labels, str(v))
	}
	user := zt["user_id"]
	if num(user) == 0 {
		var e error
		user, e = s.State.Get("zotero_user", nil)
		if e != nil {
			return nil, e
		}
	}
	out := Object{"timezone": c["timezone"], "vikunja_url": vk["url"], "vikunja_public_url": vk["public_url"], "inbox_id": vk["inbox_id"], "saved_filter_id": vk["saved_filter_id"], "readeck_url": rd["url"], "zotero_user_id": user, "readeck_trigger_labels": strings.Join(labels, ", "), "readeck_processed_label": fallback(str(rd["processed_label"]), "silverbullet-imported"), "google_calendar_enabled": boolean(gc["enabled"]), "google_calendar_interval": defaultInt(gc["interval"], 300)}
	for name, def := range map[string]int{"daily": 30, "zotero": 600, "readeck": 300} {
		out[name+"_interval"] = defaultInt(c[name+"_interval"], def)
		out[name+"_enabled"] = enabled(c, name)
	}
	return out, nil
}
func defaultInt(v any, d int) int {
	if v == nil {
		return d
	}
	return num(v)
}
func enabled(c Object, name string) bool { v, ok := obj(c["enabled"])[name]; return !ok || boolean(v) }
func (b Settings) Validate() error {
	if len([]rune(b.Timezone)) > 80 || b.Timezone == "" || len([]rune(b.VikunjaURL)) > 500 || len([]rune(b.VikunjaPublicURL)) > 500 || len([]rune(b.ReadeckURL)) > 500 || b.InboxID <= 0 || b.SavedFilterID <= 0 || b.ZoteroUserID != nil && *b.ZoteroUserID <= 0 || b.DailyInterval < 15 || b.DailyInterval > 86400 || b.ZoteroInterval < 60 || b.ZoteroInterval > 86400 || b.ReadeckInterval < 30 || b.ReadeckInterval > 86400 || b.GoogleCalendarInterval < 60 || b.GoogleCalendarInterval > 86400 || len([]rune(b.ReadeckTriggerLabels)) > 1000 || len([]rune(b.ReadeckProcessedLabel)) < 1 || len([]rune(b.ReadeckProcessedLabel)) > 100 {
		return invalid("Invalid settings")
	}
	if _, e := time.LoadLocation(b.Timezone); e != nil {
		return invalid("Invalid timezone or server URL")
	}
	for _, v := range []string{b.VikunjaURL, b.VikunjaPublicURL, b.ReadeckURL} {
		if _, e := ValidateURL(v); e != nil {
			return invalid("Invalid timezone or server URL")
		}
	}
	return nil
}
func (s *Service) SaveSettings(ctx context.Context, b Settings) (Object, error) {
	if e := b.Validate(); e != nil {
		return nil, e
	}
	vk, _ := ValidateURL(b.VikunjaURL)
	public, _ := ValidateURL(b.VikunjaPublicURL)
	rd, _ := ValidateURL(b.ReadeckURL)
	s.Lock.Lock()
	defer s.Lock.Unlock()
	for _, check := range []struct {
		changed        bool
		query, message string
	}{{vk != strings.TrimRight(str(obj(s.Config["vikunja"])["url"]), "/"), "SELECT 1 FROM tasks LIMIT 1", "Use a new connection profile/state for another Vikunja instance"}, {rd != strings.TrimRight(str(obj(s.Config["readeck"])["url"]), "/"), "SELECT 1 FROM notes WHERE source='READECK' LIMIT 1", "Use a new profile/state for another Readeck instance"}} {
		if check.changed {
			has, e := s.State.Has(check.query)
			if e != nil {
				return nil, e
			}
			if has {
				return nil, Conflict(check.message)
			}
		}
	}
	known := num(obj(s.Config["zotero"])["user_id"])
	if known == 0 {
		v, e := s.State.Get("zotero_user", nil)
		if e != nil {
			return nil, e
		}
		known = num(v)
	}
	requested := 0
	if b.ZoteroUserID != nil {
		requested = *b.ZoteroUserID
	}
	if requested != known {
		has, e := s.State.Has("SELECT 1 FROM notes WHERE source='ZOTERO' LIMIT 1")
		if e != nil {
			return nil, e
		}
		if has {
			return nil, Conflict("Keep the Zotero identity associated with existing notes")
		}
	}
	path := filepath.Join(s.SettingsDir, "integrations.json")
	config, e := readObject(path)
	if errors.Is(e, os.ErrNotExist) {
		raw, _ := json.Marshal(s.Config)
		e = json.Unmarshal(raw, &config)
	}
	if e != nil {
		return nil, e
	}
	config["timezone"] = b.Timezone
	config["daily_interval"] = b.DailyInterval
	config["zotero_interval"] = b.ZoteroInterval
	config["readeck_interval"] = b.ReadeckInterval
	labels := []any{}
	for _, v := range strings.Split(b.ReadeckTriggerLabels, ",") {
		if v = strings.TrimSpace(v); v != "" {
			labels = append(labels, v)
		}
	}
	obj(config["readeck"])["trigger_labels"] = labels
	obj(config["readeck"])["processed_label"] = b.ReadeckProcessedLabel
	gc := obj(config["google_calendar"])
	gc["enabled"] = b.GoogleCalendarEnabled
	gc["interval"] = b.GoogleCalendarInterval
	config["google_calendar"] = gc
	vkc := obj(config["vikunja"])
	vkc["url"] = vk
	vkc["public_url"] = public
	vkc["inbox_id"] = b.InboxID
	vkc["saved_filter_id"] = b.SavedFilterID
	obj(config["readeck"])["url"] = rd
	if b.ZoteroUserID == nil {
		obj(config["zotero"])["user_id"] = nil
	} else {
		obj(config["zotero"])["user_id"] = *b.ZoteroUserID
	}
	config["enabled"] = Object{"daily": b.DailyEnabled, "zotero": b.ZoteroEnabled, "readeck": b.ReadeckEnabled}
	data, e := jsonPretty(config)
	if e != nil {
		return nil, e
	}
	if e = PrivateWrite(path, []byte(data)); e != nil {
		return nil, NotConfigured("Configuration directory must be writable")
	}
	return Object{"saved": true, "restart_required": true}, nil
}
func (s *Service) SaveSecret(name, value string) (Object, error) {
	if !hasString([]string{"vikunja_token", "zotero_key", "readeck_token"}, name) {
		return nil, &Error{404, "Not Found"}
	}
	if len([]rune(value)) < 1 || len([]rune(value)) > 4096 {
		return nil, invalid("Invalid credential")
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return nil, invalid("Invalid credential")
	}
	if e := PrivateWrite(filepath.Join(s.SettingsDir, "pending-secrets", name), []byte(value+"\n")); e != nil {
		return nil, NotConfigured("Settings storage unavailable")
	}
	return Object{"saved": true, "restart_required": true}, nil
}

// LoadConfig activates only explicitly staged Relay credentials, like Python startup.
func LoadConfig(configPath, settingsDir string) (Object, error) {
	for _, name := range []string{"vikunja_token", "zotero_key", "readeck_token"} {
		path := filepath.Join(settingsDir, "pending-secrets", name)
		b, e := os.ReadFile(path)
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return nil, e
		}
		if e = PrivateWrite(filepath.Join(settingsDir, "secrets", name), b); e != nil {
			return nil, e
		}
		if e = os.Remove(path); e != nil {
			return nil, e
		}
	}
	override := filepath.Join(settingsDir, "integrations.json")
	if _, e := os.Stat(override); e == nil {
		configPath = override
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	return readObject(configPath)
}
func (s *Service) WriteEffective() error {
	user := obj(s.Config["zotero"])["user_id"]
	if num(user) == 0 {
		v, e := s.State.Get("zotero_user", nil)
		if e != nil {
			return e
		}
		user = v
	}
	b, e := jsonPretty(Object{"google_calendar": Object{"enabled": boolean(obj(s.Config["google_calendar"])["enabled"])}, "timezone": s.Config["timezone"], "connections": Object{"timezone": s.Config["timezone"], "silverbullet_url": s.Config["silverbullet_url"], "vikunja": s.Config["vikunja"], "zotero": Object{"user_id": user}}})
	if e != nil {
		return e
	}
	return PrivateWrite(filepath.Join(s.SettingsDir, "effective.json"), []byte(b))
}
