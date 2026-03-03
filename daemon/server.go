package daemon

import (
	"fmt"
	"log"
	"net/http"

	"github.com/paulchambaz/ssherd/internal"
)

type Server struct {
	mux  *http.ServeMux
	addr string
	cfg  *internal.Config
}

func NewServer(cfg *internal.Config) (*Server, error) {
	srv := &Server{
		mux:  http.NewServeMux(),
		addr: fmt.Sprintf("%s:%s", cfg.Server.Host, cfg.Server.Port),
		cfg:  cfg,
	}

	srv.registerRoutes()

	return srv, nil
}

func (s *Server) Run() {
	log.Printf("Starting server on %s", s.addr)
	if err := http.ListenAndServe(s.addr, s.mux); err != nil {
		log.Fatalf("Error when listening and serving %s", err)
	}
}


