package daemon

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
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

// BatchFormState capture l'intégralité des inputs du formulaire New Batch.
// Sérialisé dans job.FormState à la création pour permettre le Redo.
type BatchFormState struct {
	NamePrefix     string      `json:"name_prefix"`
	BaseCommand    string      `json:"base_command"`
	SeedFlag       string      `json:"seed_flag"`
	StartSeed      int         `json:"start_seed"`
	NumSeeds       int         `json:"num_seeds"`
	MaxRetries     int         `json:"max_retries"`
	MinVRAM        int         `json:"min_vram"`
	PreferredGPU   string      `json:"preferred_gpu"`
	RetrySuffix    string      `json:"retry_suffix"`
	LogArgument    string      `json:"log_argument"`
	LogPath        string      `json:"log_path"`
	OutputArgument string      `json:"output_argument"`
	OutputPath     string      `json:"output_path"`
	OutputFiles    string      `json:"output_files"`
	Axes           []axisInput `json:"axes"`
}

func (s *Server) getNewBatch(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	store, err := internal.LoadMachinesStore(s.cfg.CachePath)
	if err != nil {
		http.Error(w, "Failed to load machines", http.StatusInternalServerError)
		return
	}

	seen := make(map[string]bool)
	var gpuModels []string
	for _, m := range store.Machines {
		if m.GPUModel == "" || m.Status == internal.MachineStatusDeprecated {
			continue
		}
		if !seen[m.GPUModel] {
			seen[m.GPUModel] = true
			gpuModels = append(gpuModels, m.GPUModel)
		}
	}

	// Redo : si un query param "from" est présent, charger le FormState du job source.
	var formStateJSON string
	if from := r.URL.Query().Get("from"); from != "" {
		if sourceJob, err := internal.LoadJob(s.cfg.CachePath, p.ID, from); err == nil {
			if sourceJob.FormState != nil {
				formStateJSON = string(sourceJob.FormState)
			}
		}
	}

	if err := views.NewBatchPage(p, gpuModels, formStateJSON).Render(r.Context(), w); err != nil {
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
	startSeed, err := strconv.Atoi(r.FormValue("start_seed"))
	if err != nil || startSeed < 1 {
		startSeed = 1
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
	logArgument := strings.TrimSpace(r.FormValue("log_argument"))
	outputArgument := strings.TrimSpace(r.FormValue("output_argument"))
	outputFilesRaw := r.FormValue("output_files")

	logPathTpl := strings.TrimSpace(r.FormValue("log_path"))
	outputPathTpl := strings.TrimSpace(r.FormValue("output_path"))

	var outputFilesTpls []string
	for _, line := range strings.Split(outputFilesRaw, "\n") {
		if f := strings.TrimSpace(line); f != "" {
			outputFilesTpls = append(outputFilesTpls, f)
		}
	}

	var axes []axisInput
	if raw := r.FormValue("axes_json"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &axes); err != nil {
			http.Error(w, "Invalid axes data", http.StatusBadRequest)
			return
		}
	}

	// Sérialiser le FormState une seule fois — partagé par tous les jobs du batch.
	formState := BatchFormState{
		NamePrefix:     namePrefix,
		BaseCommand:    baseCommand,
		SeedFlag:       seedFlag,
		StartSeed:      startSeed,
		NumSeeds:       numSeeds,
		MaxRetries:     maxRetries,
		MinVRAM:        minVRAM,
		PreferredGPU:   preferredGPU,
		RetrySuffix:    retrySuffix,
		LogArgument:    logArgument,
		LogPath:        logPathTpl,
		OutputArgument: outputArgument,
		OutputPath:     outputPathTpl,
		OutputFiles:    outputFilesRaw,
		Axes:           axes,
	}
	formStateJSON, err := json.Marshal(formState)
	if err != nil {
		http.Error(w, "Failed to marshal form state", http.StatusInternalServerError)
		return
	}

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
	nfsSchedulerBase := filepath.Dir(p.RemotePath) + "/.ssherd"
	now := time.Now()

	for _, combo := range combos {
		for seed := startSeed; seed < startSeed+numSeeds; seed++ {
			ablationParts := make([]string, 0, len(combo))
			for _, v := range combo {
				ablationParts = append(ablationParts, internal.SanitizeAxisValue(v))
			}
			ablation := strings.Join(ablationParts, "_")
			if ablation == "" {
				ablation = "run"
			}

			vars := map[string]string{
				"seed":     strconv.Itoa(seed),
				"ablation": ablation,
			}
			for i, ax := range axes {
				if i < len(combo) {
					vars[ax.Name] = internal.SanitizeAxisValue(combo[i])
				}
			}

			parts := []string{namePrefix}
			for _, v := range combo {
				parts = append(parts, internal.SanitizeAxisValue(v))
			}
			parts = append(parts, "seed "+strconv.Itoa(seed))
			displayName := substituteVars(strings.Join(parts, " - "), vars)

			cmdTokens := append([]string{baseCommand}, combo...)
			cmdTokens = append(cmdTokens, seedFlag, strconv.Itoa(seed))
			baseCmd := substituteVars("cd "+p.RemotePath+" && "+strings.Join(cmdTokens, " "), vars)

			logPath := substituteVars(logPathTpl, vars)
			outputPath := substituteVars(outputPathTpl, vars)

			// Appendre les arguments log/output si configurés et que DataPath est défini.
			if logArgument != "" && logPath != "" && p.DataPath != "" {
				baseCmd += " " + logArgument + " {temporary_path}/" + p.DataPath + "/" + logPath
			}
			if outputArgument != "" && outputPath != "" && p.DataPath != "" {
				baseCmd += " " + outputArgument + " {temporary_path}/" + p.DataPath + "/" + outputPath
			}

			runCmd := baseCmd
			retryCmd := runCmd
			if retrySuffix != "" {
				retryCmd += " " + retrySuffix
			}

			// job.LogPath : avec placeholder si DataPath défini, absolu sinon.
			var jobLogPath string
			if logPath != "" && p.DataPath != "" {
				jobLogPath = "{temporary_path}/" + p.DataPath + "/" + logPath
			} else {
				jobLogPath = absOrRelative(logPath, p.RemotePath)
			}

			// job.OutputPath : relatif au DataPath si défini, absolu sinon.
			var jobOutputPath string
			if p.DataPath != "" {
				jobOutputPath = outputPath
			} else {
				jobOutputPath = absOrRelative(outputPath, p.RemotePath)
			}

			var outputFiles []string
			for _, tpl := range outputFilesTpls {
				if f := absOrRelative(substituteVars(tpl, vars), jobOutputPath); f != "" {
					outputFiles = append(outputFiles, f)
				}
			}

			id, err := internal.GenerateID()
			if err != nil {
				http.Error(w, "Failed to generate job id", http.StatusInternalServerError)
				return
			}

			job := &internal.Job{
				ID:           id,
				ProjectID:    p.ID,
				ProjectSlug:  p.Slug,
				DisplayName:  displayName,
				Status:       internal.JobPending,
				CreatedAt:    now,
				MaxRetries:   maxRetries,
				RunCommand:   runCmd,
				RetryCommand: retryCmd,
				LogPath:      jobLogPath,
				OutputPath:   jobOutputPath,
				NfsJobDir:    nfsSchedulerBase + "/jobs/" + id,
				OutputFiles:  outputFiles,
				GPURequirements: internal.GPURequirements{
					MinVRAMMB:    minVRAM,
					PreferredGPU: preferredGPU,
				},
				FormState: json.RawMessage(formStateJSON),
			}

			if err := internal.SaveJob(s.cfg.CachePath, job); err != nil {
				log.Printf("Failed to save job %s: %v", id, err)
				http.Error(w, "Failed to save job", http.StatusInternalServerError)
				return
			}
			s.scheduler.AddTask(job)
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

	localJobDir := filepath.Join(s.cfg.CachePath, p.ID, "jobs", job.ID)

	var stdoutLog, stderrLog string
	if data, err := os.ReadFile(filepath.Join(localJobDir, "stdout.log")); err == nil {
		stdoutLog = string(data)
	}
	if data, err := os.ReadFile(filepath.Join(localJobDir, "stderr.log")); err == nil {
		stderrLog = string(data)
	}

	if err := views.JobDetailPage(p, job, stdoutLog, stderrLog).Render(r.Context(), w); err != nil {
		http.Error(w, "Failed to render template", http.StatusInternalServerError)
		log.Printf("Failed to render template: %v", err)
	}
}

func (s *Server) getJobLogStdout(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	logPath := filepath.Join(s.cfg.CachePath, p.ID, "jobs", r.PathValue("id"), "stdout.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
}

func (s *Server) getJobLogStderr(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	logPath := filepath.Join(s.cfg.CachePath, p.ID, "jobs", r.PathValue("id"), "stderr.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(data)
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
	if job.Status != internal.JobRunning && job.Status != internal.JobPending {
		http.Error(w, "Job is not running or pending", http.StatusBadRequest)
		return
	}
	if err := s.scheduler.CancelJob(job.ID); err != nil {
		log.Printf("postCancelJob: %v", err)
		http.Error(w, "Failed to cancel job: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs/"+job.ID, http.StatusSeeOther)
}

func (s *Server) postCancelAllJobs(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	jobs, err := internal.LoadJobs(s.cfg.CachePath, p.ID)
	if err != nil {
		http.Error(w, "Failed to load jobs", http.StatusInternalServerError)
		return
	}

	for _, job := range jobs {
		if job.Status == internal.JobPending || job.Status == internal.JobRunning {
			if err := s.scheduler.CancelJob(job.ID); err != nil {
				log.Printf("postCancelAllJobs: cancel %s: %v", job.ID, err)
			}
		}
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs", http.StatusSeeOther)
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
	if job.Status != internal.JobFailed && job.Status != internal.JobStalled && job.Status != internal.JobCancelled {
		http.Error(w, "Job cannot be retried in its current state", http.StatusBadRequest)
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
	s.scheduler.RequeueJob(job)
	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs/"+job.ID, http.StatusSeeOther)
}

func (s *Server) postEditJob(w http.ResponseWriter, r *http.Request) {
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

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return
	}

	if cmd := strings.TrimSpace(r.FormValue("retry_command")); cmd != "" {
		job.RetryCommand = cmd
	}

	if err := internal.SaveJob(s.cfg.CachePath, job); err != nil {
		http.Error(w, "Failed to save job", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs/"+job.ID, http.StatusSeeOther)
}

func absOrRelative(path, base string) string {
	if path == "" || strings.HasPrefix(path, "/") {
		return path
	}
	return base + "/" + path
}

func substituteVars(s string, vars map[string]string) string {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}
func (s *Server) postDeleteJob(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	jobID := r.PathValue("id")
	job, err := internal.LoadJob(s.cfg.CachePath, p.ID, jobID)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	switch job.Status {
	case internal.JobDone, internal.JobFailed, internal.JobStalled, internal.JobCancelled:
		// ok
	default:
		http.Error(w, "Job is not in a terminal state", http.StatusBadRequest)
		return
	}

	if err := s.scheduler.DeleteJob(jobID, p.ID); err != nil {
		log.Printf("postDeleteJob: %v", err)
		http.Error(w, "Failed to delete job", http.StatusInternalServerError)
		return
	}

	s.scheduler.Events <- internal.JobEvent{
		Kind: internal.EventJobDeleted,
		Job:  &internal.Job{ID: jobID},
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs", http.StatusSeeOther)
}

func (s *Server) postDeleteFinishedJobs(w http.ResponseWriter, r *http.Request) {
	p, err := s.findProjectBySlug(r.PathValue("slug"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	deleted, err := s.scheduler.DeleteFinishedJobs(p.ID)
	if err != nil {
		log.Printf("postDeleteFinishedJobs: %v", err)
		http.Error(w, "Failed to delete jobs", http.StatusInternalServerError)
		return
	}

	for _, id := range deleted {
		s.scheduler.Events <- internal.JobEvent{
			Kind: internal.EventJobDeleted,
			Job:  &internal.Job{ID: id},
		}
	}

	http.Redirect(w, r, "/projects/"+p.Slug+"/jobs", http.StatusSeeOther)
}
