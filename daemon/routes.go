package daemon

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/", s.getNotFound)

	s.registerStatic()

	s.mux.HandleFunc("GET /{$}", s.getHome)
	s.mux.HandleFunc("/health", s.getHealth)

	s.mux.HandleFunc("GET /projects", s.getProjects)
	s.mux.HandleFunc("GET /projects/new", s.getNewProject)
	s.mux.HandleFunc("POST /projects", s.postProject)
	s.mux.HandleFunc("GET /projects/{slug}/edit", s.getEditProject)
	s.mux.HandleFunc("POST /projects/{slug}", s.postUpdateProject)
	s.mux.HandleFunc("POST /projects/{slug}/delete", s.postDeleteProject)

	s.mux.HandleFunc("GET /machines", s.getMachines)
	s.mux.HandleFunc("GET /machines/new", s.getNewMachines)
	s.mux.HandleFunc("POST /machines", s.postMachines)
	s.mux.HandleFunc("POST /machines/{id}/delete", s.postDeleteMachine)

	s.mux.HandleFunc("GET /proxies/new", s.getNewProxy)
	s.mux.HandleFunc("POST /proxies", s.postProxy)
	s.mux.HandleFunc("POST /proxies/{id}/delete", s.postDeleteProxy)
}
