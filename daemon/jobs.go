package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

type axisInput struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

func (s *Server) getNewBatch(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := views.NewBatchPage(p).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postBatch(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	namePrefix := strings.TrimSpace(r.FormValue("name_prefix"))
	baseCommand := strings.TrimSpace(r.FormValue("base_command"))
	if namePrefix == "" || baseCommand == "" {
		http.Error(w, "Name prefix and base command are required", http.StatusBadRequest)
		return
	}

	seedFlag := r.FormValue("seed_flag")
	if seedFlag == "" {
		seedFlag = "--seed"
	}
	numSeeds, err := strconv.Atoi(r.FormValue("num_seeds"))
	if err != nil || numSeeds < 1 {
		numSeeds = 1
	}
	maxRetries, err := strconv.Atoi(r.FormValue("max_retries"))
	if err != nil || maxRetries < 0 {
		maxRetries = 3
	}
	minVRAM, err := strconv.Atoi(r.FormValue("min_vram"))
	if err != nil || minVRAM < 0 {
		minVRAM = 2048
	}
	preferredGPU := r.FormValue("preferred_gpu")
	retrySuffix := strings.TrimSpace(r.FormValue("retry_suffix"))

	logPathTpl := strings.TrimSpace(r.FormValue("log_path"))
	outputPathTpl := strings.TrimSpace(r.FormValue("output_path"))
	logParserScript := absOrRelative(strings.TrimSpace(r.FormValue("log_parser_script")), p.RemotePath)
	vizScript := absOrRelative(strings.TrimSpace(r.FormValue("viz_script")), p.RemotePath)

	var axes []axisInput
	if raw := r.FormValue("axes_json"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &axes); err != nil {
			http.Error(w, "Invalid axes data", http.StatusBadRequest)
			return
		}
	}

	// Build axis value lists for cartesian product
	axisValues := make([][]string, 0, len(axes))
	for _, ax := range axes {
		var vals []string
		for _, v := range ax.Values {
			if v = strings.TrimSpace(v); v != "" {
				vals = append(vals, v)
			}
		}
		if len(vals) > 0 {
			axisValues = append(axisValues, vals)
		}
	}

	combos := internal.CartesianProduct(axisValues)

	nfsSchedulerBase := filepath.Dir(p.RemotePath) + "/.ppti-scheduler"
	now := time.Now()

	for _, combo := range combos {
		for seed := 1; seed <= numSeeds; seed++ {
			// substitution variables: axis name → last token of value, seed → number
			vars := map[string]string{"seed": strconv.Itoa(seed)}
			for i, ax := range axes {
				if i < len(combo) {
					vars[ax.Name] = lastToken(combo[i])
				}
			}

			// display name
			parts := []string{namePrefix}
			for _, v := range combo {
				parts = append(parts, lastToken(v))
			}
			parts = append(parts, "seed "+strconv.Itoa(seed))
			displayName := strings.Join(parts, " - ")

			// commands
			cmdTokens := append([]string{baseCommand}, combo...)
			cmdTokens = append(cmdTokens, seedFlag, strconv.Itoa(seed))
			runCmd := "cd " + p.RemotePath + " && " + strings.Join(cmdTokens, " ")
			retryCmd := runCmd
			if retrySuffix != "" {
				retryCmd += " " + retrySuffix
			}

			// paths
			logPath := absOrRelative(substituteVars(logPathTpl, vars), p.RemotePath)
			outputPath := absOrRelative(substituteVars(outputPathTpl, vars), p.RemotePath)

			id, err := internal.GenerateID()
			if err != nil {
				http.Error(w, "Failed to generate job id", http.StatusInternalServerError)
				return
			}

			job := &internal.Job{
				ID:              id,
				ProjectID:       p.ID,
				ProjectSlug:     p.Slug,
				DisplayName:     displayName,
				Status:          internal.JobPending,
				CreatedAt:       now,
				MaxRetries:      maxRetries,
				RunCommand:      runCmd,
				RetryCommand:    retryCmd,
				LogPath:         logPath,
				OutputPath:      outputPath,
				LogParserScript: logParserScript,
				VizScript:       vizScript,
				NfsJobDir:       nfsSchedulerBase + "/jobs/" + id,
				GPURequirements: internal.GPURequirements{
					MinVRAMMB:    minVRAM,
					PreferredGPU: preferredGPU,
				},
			}

			if err := internal.SaveJob(s.cfg.CachePath, job); err != nil {
				log.Printf("Failed to save job %s: %v", id, err)
				http.Error(w, "Failed to save job", http.StatusInternalServerError)
				return
			}
		}
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs", http.StatusSeeOther)
}

func (s *Server) getJobDetail(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := internal.LoadJob(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := views.JobDetailPage(p, job).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) postCancelJob(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := internal.LoadJob(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job.Status = internal.JobCancelled
	now := time.Now()
	job.FinishedAt = &now
	if err := internal.SaveJob(s.cfg.CachePath, job); err != nil {
		http.Error(w, "Failed to save job", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs/"+job.ID, http.StatusSeeOther)
}

func (s *Server) postRetryJob(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job, err := internal.LoadJob(s.cfg.CachePath, p.ID, r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	job.Status = internal.JobPending
	job.RetryCount++
	job.RunCommand = job.RetryCommand
	job.Machine = ""
	job.StartedAt = nil
	job.FinishedAt = nil
	job.Progress = nil
	if err := internal.SaveJob(s.cfg.CachePath, job); err != nil {
		http.Error(w, "Failed to save job", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs/"+job.ID, http.StatusSeeOther)
}

// absOrRelative prepends base if path is non-empty and relative.
func absOrRelative(path, base string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}
	return base + "/" + path
}

// lastToken extracts the last whitespace-separated token from s.
// "--env antmaze-large-play-v2" → "antmaze-large-play-v2"
func lastToken(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return s
	}
	return parts[len(parts)-1]
}

// substituteVars replaces {key} placeholders in s.
func substituteVars(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
