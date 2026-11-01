package hub

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDailyZoteroForwardOnly(t *testing.T) {
	state, err := OpenState(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(r.URL.Path, "/v1/paperwork/") {
			t.Fatalf("not forwarded to owner: %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"owner":"Paperwork"}`))}, nil
	})}
	service, err := NewService(Object{"timezone": "UTC", "enabled": Object{"daily": true, "zotero": true}}, state, client)
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(service)
	for _, job := range host.Jobs() {
		if job.Name == "daily" || job.Name == "zotero" {
			t.Fatalf("duplicate worker %s", job.Name)
		}
	}
	for _, path := range []string{"/v1/daily/open", "/v1/zotero/search?q=fixture"} {
		req := httptest.NewRequest("POST", path, nil)
		req.RemoteAddr = "127.0.0.1:1234"
		response := httptest.NewRecorder()
		host.Handler().ServeHTTP(response, req)
		if response.Code != 200 || !strings.Contains(response.Body.String(), "Paperwork") {
			t.Fatalf("alias failed: %d %s", response.Code, response.Body.String())
		}
	}
}
