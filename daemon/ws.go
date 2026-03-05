package daemon

import (
	"bytes"
	"log"
	"net/http"

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

    // Snapshot initial
    var buf bytes.Buffer
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
