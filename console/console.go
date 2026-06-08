// Package console serves the read-only operator web UI.
package console

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"github.com/sausheong/runtime/controlplane"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// staticFS scopes the static file server to the static/ subtree only, so an
// encoded path-traversal request (e.g. /ui/static/..%2ftemplates/...) cannot
// escape into the templates embedded alongside it.
var staticFS = mustSub(assets, "static")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Handler returns the console's HTTP handler. Read-only: it renders the agents
// overview from the registry and links to the control-plane API + SSE endpoints
// it is mounted beside.
func Handler(reg *controlplane.Registry) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(staticFS)))

	mux.HandleFunc("GET /ui/login", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "login.html", nil)
	})
	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		// HttpOnly + SameSite=Lax. Secure is intentionally NOT set so the
		// console works over plain HTTP for local/internal use; terminate TLS
		// upstream in production (and set Secure there if exposing the console).
		http.SetCookie(w, &http.Cookie{
			Name: "runtime_token", Value: r.FormValue("token"),
			Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/ui", http.StatusSeeOther)
	})

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "overview.html", map[string]any{"Agents": reg.List()})
	})

	mux.HandleFunc("GET /ui/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := reg.Get(id); !ok {
			http.NotFound(w, r)
			return
		}
		render(w, "agent.html", map[string]any{"AgentID": id})
	})

	mux.HandleFunc("GET /ui/agents/{id}/sessions/{sid}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "session.html", map[string]any{
			"AgentID":   r.PathValue("id"),
			"SessionID": r.PathValue("sid"),
		})
	})

	return mux
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
