package daemon

import (
	"encoding/json"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

type vizFormAxisInput struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type VizFormState struct {
	Name           string             `json:"name"`
	Description    string             `json:"description"`
	VizCommand     string             `json:"viz_command"`
	InputArgument  string             `json:"input_argument,omitempty"`
	InputPath      string             `json:"input_path,omitempty"`
	OutputArgument string             `json:"output_argument,omitempty"`
	OutputFile     string             `json:"output_file,omitempty"`
	BuildRemote    bool               `json:"build_remote"`
	Axes           []vizFormAxisInput `json:"axes"`
}

func (s *Server) getNewVisualization(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	var formStateJSON string
	if from := r.URL.Query().Get("from"); from != "" {
		if sourceViz, err := internal.LoadVisualization(s.cfg.CachePath, p.ID, from); err == nil {
			if sourceViz.FormState != nil {
				formStateJSON = string(sourceViz.FormState)
			}
		}
	}

	if err := views.NewVisualizationPage(p, formStateJSON).Render(r.Context(), w); err != nil {
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
	inputArgument := strings.TrimSpace(r.FormValue("input_argument"))
	inputPath := strings.TrimSpace(r.FormValue("input_path"))
	outputArgument := strings.TrimSpace(r.FormValue("output_argument"))
	outputFile := strings.TrimSpace(r.FormValue("output_file"))

	if name == "" || vizCommand == "" || outputFile == "" {
		http.Error(w, "Name, viz script and output file are required", http.StatusBadRequest)
		return
	}

	outputFileTemplate := outputFile

	type axisInputLocal struct {
		Name       string   `json:"name"`
		Flag       string   `json:"flag"`
		Values     []string `json:"values"`
		Toggleable bool     `json:"toggleable"`
	}
	var axesInput []axisInputLocal
	if raw := r.FormValue("axes_json"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &axesInput); err != nil {
			http.Error(w, "Invalid axes data", http.StatusBadRequest)
			return
		}
	}

	var axes []internal.VizAxis
	var formAxes []vizFormAxisInput
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
			formAxes = append(formAxes, vizFormAxisInput{
				Name:   strings.TrimSpace(a.Name),
				Values: vals,
			})
		}
	}

	buildRemote := r.FormValue("build_remote") == "on" || r.FormValue("build_remote") == "true"

	formState := VizFormState{
		Name:           name,
		Description:    description,
		VizCommand:     vizCommand,
		InputArgument:  inputArgument,
		InputPath:      inputPath,
		OutputArgument: outputArgument,
		OutputFile:     outputFile,
		BuildRemote:    buildRemote,
		Axes:           formAxes,
	}
	formStateJSON, err := json.Marshal(formState)
	if err != nil {
		http.Error(w, "Failed to marshal form state", http.StatusInternalServerError)
		return
	}

	id, err := internal.GenerateID()
	if err != nil {
		http.Error(w, "Failed to generate id", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	viz := &internal.Visualization{
		ID:                 id,
		ProjectID:          p.ID,
		Name:               name,
		Description:        description,
		VizCommand:         vizCommand,
		OutputFileTemplate: outputFileTemplate,
		BuildRemote:        buildRemote,
		Axes:               axes,
		InputArgument:      inputArgument,
		InputPath:          inputPath,
		OutputArgument:     outputArgument,
		CreatedAt:          now,
		UpdatedAt:          now,
		FormState:          json.RawMessage(formStateJSON),
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
	generating := r.URL.Query().Get("generating") == "true"
	if err := views.VisualizationDetailPage(p, viz, jobsWriting, generating).Render(r.Context(), w); err != nil {
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

// postGenerateVisualization lance la génération de façon asynchrone et redirige
// immédiatement vers la page de détail avec ?generating=true. L'UI affiche un
// spinner ; les événements WebSocket mettent à jour le viewer à la fin de chaque
// combo sans rechargement de page.
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

	s.scheduler.GenerateVizNow(p, viz, mode)
	http.Redirect(w, r, "/projects/"+p.Slug+"/visualizations/"+viz.ID+"?generating=true", http.StatusSeeOther)
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

func (s *Server) postUpdateVisualization(w http.ResponseWriter, r *http.Request) {
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

	if name := strings.TrimSpace(r.FormValue("name")); name != "" {
		viz.Name = name
	}
	viz.Description = strings.TrimSpace(r.FormValue("description"))
	viz.UpdatedAt = time.Now()

	if err := internal.SaveVisualization(s.cfg.CachePath, viz); err != nil {
		http.Error(w, "Failed to save visualization", http.StatusInternalServerError)
		log.Printf("Failed to save visualization: %v", err)
		return
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/visualizations/"+viz.ID, http.StatusSeeOther)
}
