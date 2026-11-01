package hub

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

func (h *Host) registerPaperworkAliases() {
	address := os.Getenv("PAPERWORK_URL")
	if address == "" {
		address = "http://127.0.0.1:8089"
	}
	target, err := url.Parse(address)
	if err != nil {
		panic(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = h.Service.Client.Transport
	director := proxy.Director
	proxy.Director = func(r *http.Request) {
		director(r)
		if !strings.HasPrefix(r.URL.Path, "/v1/paperwork/") {
			r.URL.Path = "/v1/paperwork" + r.URL.Path[len("/v1"):]
		}
		r.URL.RawPath = ""
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		Respond(w, nil, &Error{503, "Paperwork unavailable"})
	}
	h.Mux.Handle("/v1/paperwork/", proxy)
	h.Mux.Handle("/v1/daily/", proxy)
	h.Mux.Handle("/v1/zotero/", proxy)
}
