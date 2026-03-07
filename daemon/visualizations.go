package daemon

import (
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

func (s *Server) getNewVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := views.NewVisualizationPage(p).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.FormValue("name"))
	description := strings.TrimSpace(r.FormValue("description"))
	vizCommand := strings.TrimSpace(r.FormValue("viz_command"))
	outputTemplate := strings.TrimSpace(r.FormValue("output_file_template"))
	if name == "" || vizCommand == "" || outputTemplate == "" {
		http.Error(w, "Name, viz script and output template are required", http.StatusBadRequest)
		return
	}

	type axisInput struct {
		Name       string   `json:"name"`
		Flag       string   `json:"flag"`
		Values     []string `json:"values"`
		Toggleable bool     `json:"toggleable"`
	}
	var axesInput []axisInput
	if raw := r.FormValue("axes_json"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &axesInput); err != nil {
			http.Error(w, "Invalid axes data", http.StatusBadRequest)
			return
		}
	}

	var axes []internal.VizAxis
	for _, a := range axesInput {
		var vals []string
		for _, v := range a.Values {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			axes = append(axes, internal.VizAxis{
				Name:   strings.TrimSpace(a.Name),
				Values: vals,
			})
		}
	}

	id, err := internal.GenerateID()
	if err != nil {
		http.Error(w, "Failed to generate id", http.StatusInternalServerError)
		return
	}

	buildRemote := r.FormValue("build_remote") == "on" || r.FormValue("build_remote") == "true"

	now := time.Now()
	viz := &internal.Visualization{
		ID:                 id,
		ProjectID:          p.ID,
		Name:               name,
		Description:        description,
		VizCommand:         vizCommand,
		DataPath:           absOrRelative(strings.TrimSpace(r.FormValue("data_path")), p.RemotePath),
		OutputFileTemplate: outputTemplate,
		BuildRemote:        buildRemote,
		Axes:               axes,
		CreatedAt:          now,
		UpdatedAt:          now,
	}

	if err := internal.SaveVisualization(s.cfg.CachePath, viz); err != nil {
		http.Error(w, "Failed to save visualization", http.StatusInternalServerError)
		log.Printf("Failed to save visualization: %v", err)
		return
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/visualizations/"+viz.ID, http.StatusSeeOther)
}

func (s *Server) getVisualizationDetail(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	viz, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobsWriting := s.scheduler.VizJobsWriting(p, viz)
	genError := r.URL.Query().Get("gen_error")
	if err := views.VisualizationDetailPage(p, viz, jobsWriting, genError).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
	}
}

func (s *Server) getVisualizationFile(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	viz, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	selection := viz.DefaultSelection()
	for _, ax := range viz.ToggleableAxes() {
		if val := r.URL.Query().Get(ax.Name); val != "" {
			for _, v := range ax.Values {
				if v == val {
					selection[ax.Name] = val
					break
				}
			}
		}
	}

	localRepoDir := filepath.Join(s.cfg.CachePath, p.ID, "repo")
	outputPath := viz.ResolveOutputPath(localRepoDir, selection)

	if r.URL.Query().Get("format") == "png" {
		outputPath = internal.VizLocalPNGPath(outputPath)
	}

	data, err := os.ReadFile(outputPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ext := filepath.Ext(outputPath)
	contentType := mime.TypeByExtension(ext)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
}

func (s *Server) postGenerateVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	viz, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}
	mode := r.FormValue("mode")
	if mode == "" {
		mode = "auto"
	}

	ok, genErr := s.scheduler.GenerateVizNow(p, viz, mode)
	base := "/projects/" + p.Slug + "/visualizations/" + viz.ID
	if genErr != nil && ok == 0 {
		http.Redirect(w, r, base+"?gen_error="+url.QueryEscape(genErr.Error()), http.StatusSeeOther)
		return
	}
	if genErr != nil {
		log.Printf("viz: GenerateVizNow partial failure (%d ok): %v", ok, genErr)
	}
	http.Redirect(w, r, base, http.StatusSeeOther)
}

func (s *Server) postDeleteVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := internal.DeleteVisualization(s.cfg.CachePath, p.ID, r.PathValue("id")); err != nil {
		http.Error(w, "Failed to delete visualization", http.StatusInternalServerError)
		log.Printf("Failed to delete visualization: %v", err)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/visualizations", http.StatusSeeOther)
}
