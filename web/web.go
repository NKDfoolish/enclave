// Package web serves the browser UI. The static files are embedded in the API binary, so
// the UI and the API share one origin and the session cookie works without CORS.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

// Handler serves the UI with security headers. Only GET and HEAD are allowed.
func Handler() http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic("web: static directory missing from embed: " + err.Error()) // build-time mistake
	}
	fileServer := http.FileServerFS(files)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		header := w.Header()
		// Only our own scripts and styles may run, and no site may frame the UI.
		header.Set("Content-Security-Policy",
			"default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
