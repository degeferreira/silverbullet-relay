// Package hub provides Relay's backend and the public extension host.
package hub

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "modernc.org/sqlite"
)

type Object = map[string]any

// State uses the Python database verbatim; no migration renames identities.
type State struct{ DB *sql.DB }

func OpenState(path string) (*State, error) {
	db, e := sql.Open("sqlite", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA busy_timeout=10000; PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS kv(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS tasks(origin TEXT PRIMARY KEY,task_id INTEGER,phase TEXT NOT NULL,base TEXT,path TEXT,error TEXT);
 CREATE TABLE IF NOT EXISTS notes(source TEXT,identity TEXT,path TEXT,block_hash TEXT,PRIMARY KEY(source,identity));
 CREATE TABLE IF NOT EXISTS errors(key TEXT PRIMARY KEY,message TEXT,time TEXT);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return &State{db}, nil
}
func (s *State) Close() error { return s.DB.Close() }
func (s *State) Get(key string, fallback any) (any, error) {
	var raw string
	e := s.DB.QueryRow("SELECT value FROM kv WHERE key=?", key).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return fallback, nil
	}
	if e != nil {
		return nil, e
	}
	var v any
	e = json.Unmarshal([]byte(raw), &v)
	return v, e
}
func (s *State) Set(key string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT OR REPLACE INTO kv VALUES(?,?)", key, string(b))
	return e
}
func (s *State) Task(origin string) (Object, error) {
	var id sql.NullInt64
	var phase string
	var base, path, err sql.NullString
	e := s.DB.QueryRow("SELECT task_id,phase,base,path,error FROM tasks WHERE origin=?", origin).Scan(&id, &phase, &base, &path, &err)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var b any
	if base.Valid {
		if e = json.Unmarshal([]byte(base.String), &b); e != nil {
			return nil, e
		}
	}
	var n any
	if id.Valid {
		n = id.Int64
	}
	return Object{"id": n, "phase": phase, "base": b, "path": path.String, "error": nullable(err)}, nil
}
func nullable(s sql.NullString) any {
	if s.Valid {
		return s.String
	}
	return nil
}
func (s *State) SaveTask(origin string, id any, phase string, base any, path string, err any) error {
	b, e := json.Marshal(base)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT OR REPLACE INTO tasks VALUES(?,?,?,?,?,?)", origin, id, phase, string(b), path, err)
	return e
}
func (s *State) Note(source, id string) (Object, error) {
	var path, hash string
	e := s.DB.QueryRow("SELECT path,block_hash FROM notes WHERE source=? AND identity=?", source, id).Scan(&path, &hash)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return Object{"path": path, "hash": hash}, nil
}
func (s *State) SaveNote(source, id, path string, block any) error {
	b, e := BlockText(block)
	if e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT OR REPLACE INTO notes VALUES(?,?,?,?)", source, id, path, Digest(b))
	return e
}
func (s *State) Identities(source string) ([]string, error) {
	rows, e := s.DB.Query("SELECT identity FROM notes WHERE source=?", source)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if e = rows.Scan(&v); e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *State) Has(query string) (bool, error) {
	var n int
	e := s.DB.QueryRow(query).Scan(&n)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	return e == nil, e
}

type Error struct {
	Status int
	Detail string
}

func (e *Error) Error() string          { return e.Detail }
func Conflict(detail string) error      { return &Error{409, detail} }
func NotConfigured(detail string) error { return &Error{503, detail} }
func Uncertain(detail string) error     { return &Error{409, detail} }
func invalid(detail string) error       { return &Error{422, detail} }
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func obj(v any) Object {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return Object{}
}
func num(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case json.Number:
		n, _ := x.Int64()
		return int(n)
	}
	return 0
}
func boolean(v any) bool { b, _ := v.(bool); return b }
func array(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	if a, ok := v.([]string); ok {
		r := []any{}
		for _, v := range a {
			r = append(r, v)
		}
		return r
	}
	if a, ok := v.([]int); ok {
		r := []any{}
		for _, v := range a {
			r = append(r, v)
		}
		return r
	}
	return []any{}
}
func clone(m Object) Object {
	r := Object{}
	for k, v := range m {
		r[k] = v
	}
	return r
}
