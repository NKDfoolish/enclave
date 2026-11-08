# web

Browser UI for Enclave: login, workspaces, members and invitations.

Plain HTML, CSS and JavaScript with no build step. `web.go` embeds `static/` into the Core API
binary, which serves it at `/`, so the UI and API share one origin and the session cookie needs
no CORS. Open http://localhost:8080 after `make run-api`.
