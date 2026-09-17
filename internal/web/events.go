package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
)

// heartbeat keeps an idle stream alive through the proxy and lets a dead one
// be noticed. nginx closes a silent upstream connection eventually, and a
// browser holding a stream that will never speak again shows a progress bar
// that never moves.
const heartbeat = 15 * time.Second

// handleEvents streams job progress to the tab.
//
// Server-sent events rather than websockets: the traffic is one-way, the
// reconnect-and-replay behaviour is built into EventSource, and it survives
// the reverse proxy that is already there without any configuration beyond
// switching buffering off.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	since := int64(intParam(r, "since", 0))
	// The browser resends what it last saw on a reconnect; that is more
	// trustworthy than a query parameter it may have built from stale state.
	if raw := r.Header.Get("Last-Event-ID"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			since = n
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Without this nginx buffers the stream and it appears dead. It is the one
	// line that makes the difference between a working daemon and an
	// afternoon spent debugging one.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(w)
	// SSE outlives any sane write deadline, so the connection gets none and is
	// bounded by the heartbeat and the request context instead.
	_ = controller.SetWriteDeadline(time.Time{})
	flush := func() { _ = controller.Flush() }

	events, stop := s.Jobs.Subscribe(since)
	defer stop()

	// Say hello immediately, so the browser knows the stream is open even when
	// nothing is happening.
	fmt.Fprintf(w, ": connected\n\n")
	flush()

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return

		case ev, ok := <-events:
			if !ok {
				// The runner dropped us, or the daemon is shutting down.
				// Closing cleanly makes the browser reconnect on its own.
				return
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				s.Log.Warn("could not encode an event", "error", err)
				continue
			}
			fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.Seq, ev.Type, raw)
			flush()

		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flush()
		}
	}
}
