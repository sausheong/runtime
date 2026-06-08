// Package console serves the read-only operator web UI.
package console

import (
	"embed"
	"html/template"
	"net/http"

	"github.com/sausheong/runtime/controlplane"
)

//go:embed templates/*.html static/*
var assets embed.FS

var tmpl = template.Must(template.ParseFS(assets, "templates/*.html"))

// Handler returns the console's HTTP handler. Read-only: it renders the agents
// overview from the registry and links to the control-plane API + SSE endpoints
// it is mounted beside.
func Handler(reg *controlplane.Registry) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/", http.FileServerFS(assets)))

	mux.HandleFunc("GET /ui/login", func(w http.ResponseWriter, _ *http.Request) {
		render(w, "login.html", nil)
	})
	mux.HandleFunc("POST /ui/login", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		http.SetCookie(w, &http.Cookie{Name: "runtime_token", Value: r.FormValue("token"), Path: "/", HttpOnly: true})
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
