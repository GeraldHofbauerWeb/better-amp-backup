// Package web serves the backup interface that appears as a tab in AMP's own
// panel.
//
// It is same-origin with the panel: nginx routes /amp-bb/ and /Plugins/AmpBB/
// here and everything else to AMP. That is not a detail -- it is what lets the
// tab use a cookie, skip CORS entirely, and read the panel's session straight
// out of the page it is running in.
//
// Nothing here blocks. Every operation that takes longer than a request is
// handed to internal/jobs and watched over a stream.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/ops"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/snaptree"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
)

// Prefix is where the API lives. The plugin's assets live under PluginPrefix,
// which is fixed by AMP's loader rather than by us.
const (
	Prefix       = "/amp-bb"
	APIPrefix    = Prefix + "/api"
	PluginPrefix = "/Plugins/AmpBB/"
)

// Scheduler is the part of the schedule the status page needs.
type Scheduler interface {
	Next() (backup, housekeeping time.Time)
}

// Deps is everything the handler needs.
type Deps struct {
	Repo     *repo.Repository
	Settings *settings.Store
	State    *state.Store
	Jobs     *jobs.Runner
	Ops      *ops.Runner
	Trees    *snaptree.Cache
	Sessions *auth.Store
	// Validator turns a panel session into capabilities.
	Validator auth.Validator
	// UserClient builds a client that acts as the caller, for stopping and
	// starting the instance under their own identity.
	UserClient func(session string) (*amp.Client, error)
	// ServiceClient builds a client with the backup service account, for
	// anything nobody is watching.
	ServiceClient func(ctx context.Context) (*amp.Client, error)
	Scheduler     Scheduler
	Version       string
	Log           *slog.Logger
	Clock         func() time.Time
}

type server struct {
	Deps
	stats *statsCache
}

func (s *server) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

// NewHandler wires the routes.
func NewHandler(d Deps) (http.Handler, error) {
	if d.Repo == nil || d.Settings == nil || d.Jobs == nil || d.Ops == nil {
		return nil, errors.New("web: repository, settings, jobs and ops are all required")
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Sessions == nil {
		d.Sessions = auth.NewStore(0, 0, d.Clock)
	}
	if d.Trees == nil {
		d.Trees = snaptree.NewCache(0, 0, 0)
	}
	s := &server{Deps: d}
	s.stats = newStatsCache(d.Repo, d.Clock)

	mux := http.NewServeMux()

	// Unauthenticated: the unit's readiness probe, and nothing else.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	// The plugin's own files. They carry no data and are what AMP's loader
	// fetches before anyone has a session at all.
	mux.Handle("GET "+PluginPrefix, http.StripPrefix(PluginPrefix, assetHandler()))

	mux.HandleFunc("POST "+APIPrefix+"/session", s.handleExchange)
	mux.HandleFunc("DELETE "+APIPrefix+"/session", s.guard(auth.CapRead, s.handleSignOut))

	get := func(pattern string, cap auth.Capability, h handler) {
		mux.HandleFunc("GET "+APIPrefix+pattern, s.guard(cap, h))
	}
	post := func(pattern string, cap auth.Capability, h handler) {
		mux.HandleFunc("POST "+APIPrefix+pattern, s.guard(cap, h))
	}

	get("/status", auth.CapRead, s.handleStatus)
	get("/snapshots", auth.CapRead, s.handleSnapshots)
	get("/snapshots/{id}", auth.CapRead, s.handleSnapshot)
	get("/snapshots/{id}/tree", auth.CapRead, s.handleTree)
	get("/snapshots/{id}/search", auth.CapRead, s.handleSearch)
	get("/snapshots/{id}/file", auth.CapRead, s.handleFile)
	get("/settings", auth.CapRead, s.handleGetSettings)
	get("/retention/preview", auth.CapRead, s.handleRetentionPreview)
	get("/jobs", auth.CapRead, s.handleJobs)
	get("/jobs/{id}", auth.CapRead, s.handleJob)
	get("/events", auth.CapRead, s.handleEvents)

	post("/jobs/backup", auth.CapBackup, s.handleRunBackup)
	post("/jobs/restore", auth.CapRestore, s.handleRunRestore)
	post("/jobs/check", auth.CapRead, s.handleRunCheck)
	post("/jobs/housekeeping", auth.CapDestroy, s.handleRunHousekeeping)
	post("/jobs/{id}/cancel", auth.CapRead, s.handleCancel)
	post("/settings/validate", auth.CapSettings, s.handleValidateExclusions)
	post("/instance/stop", auth.CapStop, s.handleStop)
	post("/instance/start", auth.CapStart, s.handleStart)
	post("/snapshots/{id}/forget", auth.CapDestroy, s.handleForget)
	post("/snapshots/{id}/unforget", auth.CapDestroy, s.handleUnforget)

	mux.HandleFunc("PUT "+APIPrefix+"/settings", s.guard(auth.CapSettings, s.handlePutSettings))

	return s.recover(s.logRequests(mux)), nil
}

// handler is an http.HandlerFunc that also gets the caller's session.
type handler func(w http.ResponseWriter, r *http.Request, sess *auth.Session)

const (
	sessionCookie = "ampbb_session"
	csrfHeader    = "X-AMPBB-CSRF"
	maxBody       = 1 << 20
)

// guard resolves the session, checks CSRF on anything that changes something,
// and then the capability.
func (s *server) guard(cap auth.Capability, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)

		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "no session", "sign in through the panel")
			return
		}
		sess, ok := s.Sessions.Lookup(cookie.Value)
		if !ok {
			clearSessionCookie(w, r)
			writeError(w, http.StatusUnauthorized, "session expired", "sign in through the panel")
			return
		}
		if r.Method != http.MethodGet && !sess.CheckCSRF(r.Header.Get(csrfHeader)) {
			writeError(w, http.StatusForbidden, "bad csrf token", "reload the tab")
			return
		}
		if !sess.Can(cap) {
			// Naming the capability is the difference between a button that
			// does nothing and a sentence explaining which AMP permission is
			// missing.
			writeError(w, http.StatusForbidden, "not allowed",
				fmt.Sprintf("this AMP account has no %q permission here", cap))
			return
		}
		h(w, r, sess)
	}
}

// recover turns a handler panic into a 500 rather than the end of the daemon,
// which also runs the scheduler.
func (s *server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.Log.Error("handler panicked", "path", r.URL.Path, "panic", p)
				writeError(w, http.StatusInternalServerError, "internal error", "")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := s.now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		// The stream is long-lived and logging its duration on close would say
		// nothing useful; everything else gets a line.
		if strings.HasSuffix(r.URL.Path, "/events") {
			return
		}
		s.Log.Info("request",
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration", s.now().Sub(started).Round(time.Millisecond))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.NewResponseController reach the real writer, which the
// stream needs in order to flush.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// errorBody is what every failure looks like, so the tab has one thing to
// render rather than a different shape per endpoint.
type errorBody struct {
	Error  string `json:"error"`
	Detail string `json:"detail,omitempty"`
	// Reason is a stable machine-readable tag for the cases the interface has
	// to react to rather than merely display.
	Reason string    `json:"reason,omitempty"`
	State  string    `json:"state,omitempty"`
	Job    *jobs.Job `json:"job,omitempty"`
}

func writeError(w http.ResponseWriter, code int, msg, detail string) {
	writeJSON(w, code, errorBody{Error: msg, Detail: detail})
}

// writeOperationError maps the errors the operations layer raises onto the
// responses the interface knows how to act on.
func writeOperationError(w http.ResponseWriter, err error) {
	var running *ops.ErrInstanceRunning
	if errors.As(err, &running) {
		writeJSON(w, http.StatusConflict, errorBody{
			Error:  err.Error(),
			Reason: "instance_running",
			State:  running.State,
		})
		return
	}
	if errors.Is(err, jobs.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such job", "")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error(), "")
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: Prefix, MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isTLS(r),
	})
}

// isTLS reports whether the browser reached us over HTTPS. nginx terminates
// TLS and tells us with a header; trusting it is safe because nothing but
// nginx can reach the socket.
func isTLS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
