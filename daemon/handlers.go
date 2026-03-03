package daemon

import (
	"log"
	"net/http"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/static"
	"github.com/paulchambaz/ssherd/views"
)

func (s *Server) getHome(w http.ResponseWriter, r *http.Request) {
	projects, err := internal.LoadProjects(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load projects", http.StatusInternalServerError)
		log.Printf("Failed to load projects: %v", err)
		return
	}
	if err := views.Home(projects).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
func (s *Server) getNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
	if err := views.NotFound().Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}
func (s *Server) registerStatic() {
	fs := http.FileServer(static.FileSystem())
	staticHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Mode == "dev" {
			w.Header().Set("Cache-Control", "public, max-age=0")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=63072000")
		}
		http.StripPrefix("/static/", fs).ServeHTTP(w, r)
	})
	s.mux.Handle("GET /static/", staticHandler)
}
