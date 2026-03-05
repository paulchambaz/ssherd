package daemon

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg := s.scheduler.GetConfig()
	if err := views.SettingsPage(cfg).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
	}
}

func (s *Server) postSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	cfg := s.scheduler.GetConfig()

	if v, err := strconv.ParseFloat(r.FormValue("use_ratio"), 64); err == nil && v > 0 && v <= 100 {
		cfg.UseRatio = v / 100
	}
	if d, err := time.ParseDuration(r.FormValue("dispatch_interval")); err == nil && d > 0 {
		cfg.DispatchInterval = d
	}
	if d, err := time.ParseDuration(r.FormValue("monitor_interval")); err == nil && d > 0 {
		cfg.MonitorInterval = d
	}
	if d, err := time.ParseDuration(r.FormValue("stall_timeout")); err == nil && d > 0 {
		cfg.StallTimeout = d
	}
	if d, err := time.ParseDuration(r.FormValue("sync_interval")); err == nil && d > 0 {
		cfg.SyncInterval = d
	}
	if d, err := time.ParseDuration(r.FormValue("viz_interval")); err == nil && d > 0 {
		cfg.VizInterval = d
	}
	if v := strings.TrimSpace(r.FormValue("local_prefix")); true {
		cfg.LocalPrefix = v
	}

	if err := s.scheduler.UpdateConfig(cfg); err != nil {
		http.Error(w, "Failed to save config", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
