package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
)

type Server struct {
	mux       *http.ServeMux
	addr      string
	cfg       *internal.Config
	scheduler *internal.Scheduler
	hub       *internal.Hub
}

func NewServer(cfg *internal.Config) (*Server, error) {
	scheduler, err := internal.NewScheduler(internal.DefaultSchedulerConfig(), cfg.CachePath)
	if err != nil {
		return nil, fmt.Errorf("init scheduler: %w", err)
	}

	srv := &Server{
		mux:       http.NewServeMux(),
		addr:      fmt.Sprintf("%s:%s", cfg.Server.Host, cfg.Server.Port),
		cfg:       cfg,
		scheduler: scheduler,
		hub:       internal.NewHub(),
	}

	srv.registerRoutes()
	return srv, nil
}

func (s *Server) Run() {
	s.scheduler.Start()
	go s.relayEvents()
	log.Printf("Starting server on %s", s.addr)
	if err := http.ListenAndServe(s.addr, s.mux); err != nil {
		log.Fatalf("Error when listening and serving: %s", err)
	}
}

// relayEvents lit le canal Events du scheduler, rend les fragments HTML et les
// diffuse à tous les clients WebSocket connectés. Les deux fragments sont
// envoyés dans un seul message : htmx-ext-ws applique les deux OOB swaps en
// une passe, ce qui évite des flash visuels.
//
// Les clients dont le DOM ne contient pas l'élément cible ignorent silencieusement
// le swap (comportement natif de htmx hx-swap-oob).
func (s *Server) relayEvents() {
	for event := range s.scheduler.Events {
		var buf bytes.Buffer
		// Fragment liste (page /jobs) — cible : #job-row-{id}
		if err := views.JobRowFragment(event.Job).Render(context.Background(), &buf); err != nil {
			log.Printf("relay: render job row: %v", err)
			continue
		}
		// Fragment progression (page /jobs/{id}) — cible : #job-progress-{id}
		if err := views.JobProgressFragment(event.Job).Render(context.Background(), &buf); err != nil {
			log.Printf("relay: render job progress: %v", err)
			continue
		}
		if event.StdoutLog != "" || event.StderrLog != "" {
			if err := views.JobLogsFragment(event.Job, event.StdoutLog, event.StderrLog).Render(context.Background(), &buf); err != nil {
				log.Printf("relay: render job logs: %v", err)
			}
		}
		s.hub.Broadcast(buf.String())
	}
}
