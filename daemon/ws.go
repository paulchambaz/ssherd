package daemon

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/paulchambaz/ssherd/internal"
	"github.com/paulchambaz/ssherd/views"
	"nhooyr.io/websocket"
)

func (s *Server) getWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true,
	})
	if err != nil {
		log.Printf("ws: accept: %v", err)
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(r.Context())
	ch := s.hub.Subscribe()
	defer s.hub.Unsubscribe(ch)

	var buf bytes.Buffer

	// ── Snapshot jobs ────────────────────────────────────────────────────────
	for _, job := range s.scheduler.Snapshot() {
		buf.Reset()
		if err := views.JobRowFragment(job).Render(ctx, &buf); err != nil {
			continue
		}
		if err := views.JobProgressFragment(job).Render(ctx, &buf); err != nil {
			continue
		}
		if err := conn.Write(ctx, websocket.MessageText, buf.Bytes()); err != nil {
			return
		}
	}

	// ── Snapshot viz ─────────────────────────────────────────────────────────
	// Pour chaque combo de chaque viz :
	//   - si en cours de génération  → envoyer "generating"
	//   - si le fichier existe       → envoyer "" (succès)
	//   - sinon                      → ne rien envoyer (pas encore généré)
	//
	// Le client sur la page de détail d'une viz recevra les fragments
	// correspondant à sa viz et mettra à jour son viewer. Les autres pages
	// ignorent silencieusement les swaps OOB sur des ids inconnus.
	projects, err := internal.LoadProjects(s.cfg.CachePath)
	if err == nil {
		for _, p := range projects {
			vizs, err := internal.LoadVisualizations(s.cfg.CachePath, p.ID)
			if err != nil {
				continue
			}
			localRepoDir := filepath.Join(s.cfg.CachePath, p.ID, "repo")

			for _, viz := range vizs {
				for _, combo := range viz.AllCombos() {
					var vizErr string

					if s.scheduler.IsVizGenerating(viz.ID, combo.Key) {
						vizErr = "generating"
					} else {
						// Résoudre le chemin de sortie pour ce combo.
						sel := map[string]string{}
						for i, ax := range viz.Axes {
							if i < len(combo.Args) {
								sel[ax.Name] = combo.Args[i]
							}
						}
						outputPath := viz.ResolveOutputPath(localRepoDir, sel)
						if _, statErr := os.Stat(outputPath); statErr != nil {
							// Fichier absent et pas en génération : on ne
							// signale rien, le placeholder reste affiché.
							continue
						}
						vizErr = ""
					}

					buf.Reset()
					if err := views.VizResultFragment(viz.ID, combo.Key, vizErr).Render(ctx, &buf); err != nil {
						continue
					}
					if err := conn.Write(ctx, websocket.MessageText, buf.Bytes()); err != nil {
						return
					}
				}
			}
		}
	}

	// ── Boucle principale ────────────────────────────────────────────────────
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, []byte(msg)); err != nil {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
