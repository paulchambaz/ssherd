package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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
	vizScript := strings.TrimSpace(r.FormValue("viz_script"))
	if name == "" || vizScript == "" {
		http.Error(w, "Name and viz script are required", http.StatusBadRequest)
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
				Name:       strings.TrimSpace(a.Name),
				Flag:       strings.TrimSpace(a.Flag),
				Values:     vals,
				Toggleable: a.Toggleable,
			})
		}
	}

	id, err := internal.GenerateID()
	if err != nil {
		http.Error(w, "Failed to generate id", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	viz := &internal.Visualization{
		ID:        id,
		ProjectID: p.ID,
		Name:      name,
		VizScript: absOrRelative(vizScript, p.RemotePath),
		DataPath:  absOrRelative(strings.TrimSpace(r.FormValue("data_path")), p.RemotePath),
		Axes:      axes,
		CreatedAt: now,
		UpdatedAt: now,
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
	if err := views.VisualizationDetailPage(p, viz).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getVisualizationSVG(w http.ResponseWriter, r *http.Request) {
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

	// Build selection from URL query params
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

	key := viz.ComboKey(selection)
	svgPath := internal.VizSVGPath(s.cfg.CachePath, p.ID, viz.ID, key)

	data, err := os.ReadFile(svgPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
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

func (s *Server) postGenerateVisualization(w http.ResponseWriter, r *http.Request) {
	// Stub — generation will be triggered by the scheduler once SSH is wired.
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/visualizations/"+r.PathValue("id"), http.StatusSeeOther)
}
