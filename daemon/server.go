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

// relayEvents lit le canal Events du scheduler et diffuse les fragments HTML
// à tous les clients WebSocket connectés.
//
// Pour les événements de jobs (status, progress) : deux fragments sont envoyés
// dans un seul message — JobRowFragment (liste) et JobProgressFragment (détail).
// Les clients sur d'autres pages ignorent silencieusement le swap OOB.
//
// Pour les événements de viz (viz_done) : un VizResultFragment est envoyé.
// Il cible #viz-result-{vizID} qui n'existe que sur la page de détail de cette
// viz ; les autres pages ignorent le swap.
func (s *Server) relayEvents() {
	for event := range s.scheduler.Events {
		var buf bytes.Buffer

		if event.Kind == internal.EventVizDone {
			if err := views.VizResultFragment(event.VizID, event.ComboKey, event.VizErr).Render(context.Background(), &buf); err != nil {
				log.Printf("relay: render viz result: %v", err)
				continue
			}
			s.hub.Broadcast(buf.String())
			continue
		}

		// Événements de jobs
		if err := views.JobRowFragment(event.Job).Render(context.Background(), &buf); err != nil {
			log.Printf("relay: render job row: %v", err)
			continue
		}
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
