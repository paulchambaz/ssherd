package daemon

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

func (s *Server) getProjectDashboard(w http.ResponseWriter, r *http.Request) {
	s.renderProjectTab(w, r, internal.TabDashboard)
}

func (s *Server) getProjectJobs(w http.ResponseWriter, r *http.Request) {
	s.renderProjectTab(w, r, internal.TabJobs)
}

func (s *Server) getProjectVisualizations(w http.ResponseWriter, r *http.Request) {
	s.renderProjectTab(w, r, internal.TabVisualizations)
}

func (s *Server) getProjectFiles(w http.ResponseWriter, r *http.Request) {
	s.renderProjectTab(w, r, internal.TabFiles)
}

func (s *Server) getProjectSettings(w http.ResponseWriter, r *http.Request) {
	s.renderProjectTab(w, r, internal.TabSettings)
}

func (s *Server) renderProjectTab(w http.ResponseWriter, r *http.Request, tab internal.ProjectTab) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	data := internal.ProjectPageData{Project: p, Tab: tab}

	if tab == internal.TabJobs || tab == internal.TabDashboard {
		jobs, err := internal.LoadJobs(s.cfg.CachePath, p.ID)
		if err != nil {
			log.Printf("Failed to load jobs for project %s: %v", p.ID, err)
		}
		data.Jobs = jobs
	}

	if tab == internal.TabVisualizations || tab == internal.TabDashboard {
		vizs, err := internal.LoadVisualizations(s.cfg.CachePath, p.ID)
		if err != nil {
			log.Printf("Failed to load visualizations for project %s: %v", p.ID, err)
		}
		data.Visualizations = vizs
	}

	if tab == internal.TabFiles {
		subPath := r.URL.Query().Get("path")
		repoDir := filepath.Join(s.cfg.CachePath, p.ID, "repo")
		files, err := internal.ListRepoDir(repoDir, subPath)
		if err != nil {
			log.Printf("Failed to list repo dir for project %s: %v", p.ID, err)
		}
		data.RepoFiles = files
		data.RepoSubPath = subPath
	}

	if err := views.ProjectPage(data).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getFileDownload(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	subPath := r.URL.Query().Get("path")
	if subPath == "" {
		http.Error(w, "Missing path", http.StatusBadRequest)
		return
	}
	// Empêcher les path traversal
	repoDir := filepath.Join(s.cfg.CachePath, p.ID, "repo")
	fullPath := filepath.Join(repoDir, subPath)
	if !strings.HasPrefix(fullPath, repoDir) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(fullPath))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, fullPath)
}
