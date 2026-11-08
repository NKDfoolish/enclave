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
// uploadOrigin is the object storage origin the UI may send presigned uploads to.
func Handler(uploadOrigin string) http.Handler {
	files, err := fs.Sub(static, "static")
	if err != nil {
		panic("web: static directory missing from embed: " + err.Error()) // build-time mistake
	}
	fileServer := http.FileServerFS(files)
	// Only our own scripts and styles may run, no site may frame the UI, and fetch() may only
	// reach the API and the storage origin used for presigned uploads.
	csp := "default-src 'self'; connect-src 'self' " + uploadOrigin +
		"; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		header := w.Header()
		header.Set("Content-Security-Policy", csp)
		header.Set("X-Content-Type-Options", "nosniff")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
