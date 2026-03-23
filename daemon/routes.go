package daemon

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.getNotFound)

	s.registerStatic()

	s.mux.HandleFunc("GET /{$}", s.getHome)
	s.mux.HandleFunc("/health", s.getHealth)
	s.mux.HandleFunc("GET /ws", s.getWS)
	s.mux.HandleFunc("GET /share/{viz_id}", s.getSharedVisualization)
	s.mux.HandleFunc("GET /share/file/{filename}", s.getSharedFile)

	s.mux.HandleFunc("GET /projects", s.getProjects)
	s.mux.HandleFunc("GET /projects/new", s.getNewProject)
	s.mux.HandleFunc("POST /projects", s.postProject)
	s.mux.HandleFunc("GET /projects/{slug}", s.getProjectDashboard)
	s.mux.HandleFunc("GET /projects/{slug}/edit", s.getEditProject)
	s.mux.HandleFunc("POST /projects/{slug}", s.postUpdateProject)
	s.mux.HandleFunc("POST /projects/{slug}/delete", s.postDeleteProject)

	s.mux.HandleFunc("GET /projects/{slug}/jobs", s.getProjectJobs)
	s.mux.HandleFunc("GET /projects/{slug}/jobs/new", s.getNewBatch)
	s.mux.HandleFunc("POST /projects/{slug}/jobs", s.postBatch)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/cancel-all", s.postCancelAllJobs)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/delete-finished", s.postDeleteFinishedJobs)
	s.mux.HandleFunc("GET /projects/{slug}/jobs/{id}", s.getJobDetail)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/{id}/cancel", s.postCancelJob)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/{id}/retry", s.postRetryJob)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/{id}/delete", s.postDeleteJob)
	s.mux.HandleFunc("POST /projects/{slug}/jobs/{id}/edit", s.postEditJob)
	s.mux.HandleFunc("GET /projects/{slug}/jobs/{id}/logs/stdout", s.getJobLogStdout)
	s.mux.HandleFunc("GET /projects/{slug}/jobs/{id}/logs/stderr", s.getJobLogStderr)

	s.mux.HandleFunc("GET /projects/{slug}/visualizations", s.getProjectVisualizations)
	s.mux.HandleFunc("GET /projects/{slug}/visualizations/new", s.getNewVisualization)
	s.mux.HandleFunc("POST /projects/{slug}/visualizations", s.postVisualization)
	s.mux.HandleFunc("GET /projects/{slug}/visualizations/{id}", s.getVisualizationDetail)
	s.mux.HandleFunc("POST /projects/{slug}/visualizations/{id}", s.postUpdateVisualization)
	s.mux.HandleFunc("GET /projects/{slug}/visualizations/{id}/file", s.getVisualizationFile)
	s.mux.HandleFunc("POST /projects/{slug}/visualizations/{id}/generate", s.postGenerateVisualization)
	s.mux.HandleFunc("POST /projects/{slug}/visualizations/{id}/share", s.postShareVisualization)
	s.mux.HandleFunc("POST /projects/{slug}/visualizations/{id}/delete", s.postDeleteVisualization)

	s.mux.HandleFunc("GET /projects/{slug}/files", s.getProjectFiles)
	s.mux.HandleFunc("POST /projects/{slug}/files/sync", s.postSyncFiles)
	s.mux.HandleFunc("GET /projects/{slug}/files/download", s.getFileDownload)
	s.mux.HandleFunc("GET /projects/{slug}/settings", s.getProjectSettings)

	s.mux.HandleFunc("GET /machines", s.getMachines)
	s.mux.HandleFunc("GET /machines/new", s.getNewMachines)
	s.mux.HandleFunc("POST /machines", s.postMachines)
	s.mux.HandleFunc("POST /machines/{id}/delete", s.postDeleteMachine)
	s.mux.HandleFunc("POST /machines/{id}/reset", s.postResetMachine) // nouveau

	s.mux.HandleFunc("GET /proxies/new", s.getNewProxy)
	s.mux.HandleFunc("POST /proxies", s.postProxy)
	s.mux.HandleFunc("POST /proxies/{id}/delete", s.postDeleteProxy)

	s.mux.HandleFunc("GET /settings", s.getSettings)
	s.mux.HandleFunc("POST /settings", s.postSettings)
}
