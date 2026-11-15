// Package web serves the UI and the JSON API.
package web

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"mime"
	"net/http"

	"github.com/503stack/trawl/internal/config"
	"github.com/503stack/trawl/internal/queue"
)

//go:embed index.html
var index []byte

//go:embed favicon.svg
var favicon []byte

//go:embed apple-touch-icon.png
var touchIcon []byte

// asset serves an embedded file that only changes with a new release.
func asset(ctype string, data []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(data)
	}
}

// Handler returns the HTTP handler for the UI and API.
func Handler(m *queue.Manager, cfg *config.Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(index)
	})
	mux.HandleFunc("GET /favicon.svg", asset("image/svg+xml", favicon))
	mux.HandleFunc("GET /apple-touch-icon.png", asset("image/png", touchIcon))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"folders": cfg.Folders, "verify": cfg.VerifyLevel, "dedupe": cfg.Dedupe})
	})
	mux.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, m.List())
	})
	mux.HandleFunc("POST /api/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req queue.Request
		if !readJSON(w, r, &req) {
			return
		}
		j, err := m.Add(req)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, j)
	})
	mux.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if readJSON(w, r, &struct{}{}) {
			reply(w, m.Cancel(r.PathValue("id")))
		}
	})
	mux.HandleFunc("POST /api/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			URL string `json:"url"`
		}
		if readJSON(w, r, &body) {
			reply(w, m.Retry(r.PathValue("id"), body.URL))
		}
	})
	mux.HandleFunc("POST /api/jobs/{id}/keep", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name string `json:"name"`
		}
		if readJSON(w, r, &body) {
			reply(w, m.Keep(r.PathValue("id"), body.Name))
		}
	})
	mux.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		reply(w, m.Remove(r.PathValue("id")))
	})
	mux.HandleFunc("POST /api/clear", func(w http.ResponseWriter, r *http.Request) {
		if readJSON(w, r, &struct{}{}) {
			m.ClearFinished()
			w.WriteHeader(http.StatusNoContent)
		}
	})

	var h http.Handler = mux
	if cfg.Password != "" {
		h = basicAuth(h, cfg.Password)
	}
	return h
}

// basicAuth accepts any user name with the configured password.
func basicAuth(next http.Handler, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			_, pw, ok := r.BasicAuth()
			if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(password)) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="trawl"`)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// readJSON requires a JSON body. Requiring application/json also means a
// browser cannot send these requests cross-site without a CORS preflight.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		http.Error(w, "bad JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	if queue.IsNotFound(err) {
		code = http.StatusNotFound
	}
	http.Error(w, err.Error(), code)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
