package daemon

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

func (s *Server) getProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := internal.LoadProjects(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load projects", http.StatusInternalServerError)
		log.Printf("Failed to load projects: %v", err)
		return
	}
	if err := views.Projects(projects).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getNewProject(w http.ResponseWriter, r *http.Request) {
	if err := views.NewProject().Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postProject(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "Name is required", http.StatusBadRequest)
		return
	}

	id, err := internal.GenerateID()
	if err != nil {
		http.Error(w, "Failed to generate project id", http.StatusInternalServerError)
		return
	}

	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	p := &internal.Project{
		ID:         id,
		Name:       name,
		Slug:       internal.Slugify(name),
		RemotePath: strings.TrimSpace(r.FormValue("remote_path")),
		GitRepo:    strings.TrimSpace(r.FormValue("git_repo")),
		Branch:     branch,
		GitToken:   strings.TrimSpace(r.FormValue("git_token")),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	if err := internal.SaveProject(s.cfg.CachePath, p); err != nil {
		http.Error(w, "Failed to save project", http.StatusInternalServerError)
		log.Printf("Failed to save project: %v", err)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) getEditProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := views.EditProject(p).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postUpdateProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	branch := r.FormValue("branch")
	if branch == "" {
		branch = "master"
	}

	p.Name = strings.TrimSpace(r.FormValue("name"))
	p.Slug = internal.Slugify(p.Name)
	p.RemotePath = strings.TrimSpace(r.FormValue("remote_path"))
	p.GitRepo = strings.TrimSpace(r.FormValue("git_repo"))
	p.Branch = branch
	p.GitToken = strings.TrimSpace(r.FormValue("git_token"))
	p.UpdatedAt = time.Now()

	if err := internal.SaveProject(s.cfg.CachePath, p); err != nil {
		http.Error(w, "Failed to save project", http.StatusInternalServerError)
		log.Printf("Failed to save project: %v", err)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postDeleteProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := internal.DeleteProject(s.cfg.CachePath, p.ID); err != nil {
		http.Error(w, "Failed to delete project", http.StatusInternalServerError)
		log.Printf("Failed to delete project: %v", err)
		return
	}
	http.Redirect(w, r, "/projects", http.StatusSeeOther)
}

func (s *Server) postSyncFiles(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.scheduler.SyncRepoNow(p.ID); err != nil {
		http.Error(w, "Sync failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/files", http.StatusSeeOther)
}

func (s *Server) findProjectBySlug(slug string) (*internal.Project, error) {
	projects, err := internal.LoadProjects(s.cfg.CachePath)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if p.Slug == slug {
			return p, nil
		}
	}
	return nil, fmt.Errorf("project not found: %s", slug)
}
