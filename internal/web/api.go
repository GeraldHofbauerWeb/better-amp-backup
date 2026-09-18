package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/ops"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
)

// --- sessions ---------------------------------------------------------------

type exchangeRequest struct {
	// Session is localStorage["LastSessionID"], read by the plugin from the
	// page it is running in.
	Session string `json:"session"`
	// User is what the panel calls this person. It decides nothing.
	User string `json:"user"`
}

type exchangeResponse struct {
	CSRF         string                   `json:"csrf"`
	User         string                   `json:"user"`
	UserVerified bool                     `json:"user_verified"`
	Capabilities map[auth.Capability]bool `json:"capabilities"`
}

func (s *server) handleExchange(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req exchangeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	sess, err := s.Sessions.Exchange(r.Context(), req.Session, req.User, s.Validator)
	if err != nil {
		// One shape of answer whatever went wrong. Telling an anonymous caller
		// which part of their guess was wrong is free help.
		writeError(w, http.StatusUnauthorized, "session rejected",
			"AMP does not recognise that session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sess.ID, Path: Prefix,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isTLS(r),
	})
	writeJSON(w, http.StatusOK, exchangeResponse{
		CSRF: sess.CSRF, User: sess.User, UserVerified: sess.UserVerified,
		Capabilities: sess.Caps,
	})
}

func (s *server) handleSignOut(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	s.Sessions.Revoke(sess.ID)
	clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// --- status -----------------------------------------------------------------

// Status is the tab's home screen. It has to answer in milliseconds, so
// everything expensive in it is cached and carries its own age.
type Status struct {
	Instance struct {
		Name  string `json:"name"`
		State string `json:"state"`
		// Error records that AMP could not be reached. It is reported rather
		// than raised: the snapshots are still there and still restorable, and
		// a status page that 500s because a panel is restarting is useless.
		Error string `json:"error,omitempty"`
	} `json:"instance"`

	Repo struct {
		Path            string         `json:"path"`
		Stats           repo.RepoStats `json:"stats"`
		Space           repo.SpaceInfo `json:"space"`
		StatsAgeSeconds int            `json:"stats_age_seconds"`
	} `json:"repo"`

	// LastSnapshot is the newest snapshot in the repository, whoever produced
	// it. LastBackup is only what this daemon did, and after a migration from
	// the systemd timers that is nothing at all -- so the overview would
	// announce "never" over a repository holding fifty snapshots.
	LastSnapshot *repo.Manifest `json:"last_snapshot,omitempty"`

	LastBackup    *state.Run  `json:"last_backup,omitempty"`
	LastRestore   *state.Run  `json:"last_restore,omitempty"`
	LastHousekeep *state.Run  `json:"last_housekeeping,omitempty"`
	LastSkip      *state.Skip `json:"last_skip,omitempty"`

	NextBackup       *time.Time `json:"next_backup,omitempty"`
	NextHousekeeping *time.Time `json:"next_housekeeping,omitempty"`

	CurrentJob *jobs.Job `json:"current_job,omitempty"`

	// AMPBackups reports AMP's own backup schedule when it is still running.
	// Two schedules backing up one instance is the single most expensive
	// misconfiguration available here, and nothing else would ever mention it.
	AMPBackups *ampSchedule `json:"amp_backups,omitempty"`

	Capabilities map[auth.Capability]bool `json:"capabilities"`
	User         string                   `json:"user"`
	UserVerified bool                     `json:"user_verified"`
	Version      string                   `json:"version"`
	// ServerNow lets the browser render relative times against our clock
	// rather than the viewer's, when the two disagree.
	ServerNow time.Time `json:"server_now"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	cfg := s.Settings.Get()
	var out Status
	out.Instance.Name = cfg.Instance.Name
	out.Repo.Path = s.Repo.Root()
	out.Capabilities = sess.Caps
	out.User = sess.User
	out.UserVerified = sess.UserVerified
	out.Version = s.Version
	out.ServerNow = s.now().UTC()

	stats, age := s.stats.get(r.Context())
	out.Repo.Stats = stats
	out.Repo.StatsAgeSeconds = int(age.Seconds())
	if space, err := s.Repo.Space(); err == nil {
		out.Repo.Space = space
	}

	if st := s.stateOrEmpty(); true {
		out.LastBackup, out.LastRestore = st.LastBackup, st.LastRestore
		out.LastHousekeep, out.LastSkip = st.LastHousekeep, st.LastSkip
	}
	if job, ok := s.Jobs.Current(); ok {
		out.CurrentJob = &job
	}
	out.LastSnapshot = s.newestSnapshot(cfg.Instance.Name)
	out.NextBackup, out.NextHousekeeping = nextRuns(s.Scheduler)

	out.Instance.State = s.instanceState(r.Context(), &out)
	out.AMPBackups = s.ampBackupSchedule(r.Context(), sess)

	writeJSON(w, http.StatusOK, out)
}

// newestSnapshot is the most recent snapshot for an instance, by the time the
// run started rather than by id -- an id is only accurate to the second.
func (s *server) newestSnapshot(instance string) *repo.Manifest {
	all, err := s.Repo.ListSnapshots()
	if err != nil {
		s.Log.Warn("could not list snapshots for the status page", "error", err)
		return nil
	}
	var newest *repo.Manifest
	for i := range all {
		m := all[i]
		if instance != "" && !strings.EqualFold(m.Instance, instance) {
			continue
		}
		if newest == nil || m.StartedAt.After(newest.StartedAt) {
			newest = &m
		}
	}
	return newest
}

func (s *server) stateOrEmpty() state.State {
	if s.State == nil {
		return state.State{}
	}
	return s.State.Get()
}

func (s *server) instanceState(ctx context.Context, out *Status) string {
	if s.ServiceClient == nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	client, err := s.ServiceClient(ctx)
	if err != nil {
		out.Instance.Error = err.Error()
		return "unknown"
	}
	status, err := client.GetStatus(ctx)
	if err != nil {
		out.Instance.Error = err.Error()
		return "unknown"
	}
	return status.State.String()
}

// --- snapshots --------------------------------------------------------------

func (s *server) handleSnapshots(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	all, err := s.Repo.ListSnapshots()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	instance := r.URL.Query().Get("instance")
	out := make([]repo.Manifest, 0, len(all))
	for _, m := range all {
		if instance != "" && !strings.EqualFold(m.Instance, instance) {
			continue
		}
		out = append(out, m)
	}
	// Newest first, by the time the run actually started rather than by id.
	// A snapshot id is only accurate to the second, so two runs inside one
	// second would otherwise come back in the order of their random suffixes.
	slices.SortStableFunc(out, func(a, b repo.Manifest) int {
		if c := b.StartedAt.Compare(a.StartedAt); c != 0 {
			return c
		}
		return strings.Compare(b.ID, a.ID)
	})
	if limit := intParam(r, "limit", 0); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": out})
}

func (s *server) handleSnapshot(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	m, err := s.Repo.LoadManifest(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *server) handleTree(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	tree, err := s.Trees.Get(r.Context(), s.Repo, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot", err.Error())
		return
	}
	dir := r.URL.Query().Get("path")
	nodes, total, ok := tree.List(dir, intParam(r, "offset", 0), intParam(r, "limit", 500))
	if !ok {
		writeError(w, http.StatusNotFound, "no such directory", dir)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": dir, "entries": nodes, "total": total,
		"snapshot_entries": tree.Entries(), "snapshot_bytes": tree.Bytes(),
	})
}

func (s *server) handleSearch(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	tree, err := s.Trees.Get(r.Context(), s.Repo, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": tree.Search(r.URL.Query().Get("q"), intParam(r, "limit", 100)),
	})
}

// handleFile streams one file out of a snapshot, so that "is this the world I
// think it is" can be answered without restoring anything.
func (s *server) handleFile(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	tree, err := s.Trees.Get(r.Context(), s.Repo, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot", err.Error())
		return
	}
	node, ok := tree.Stat(r.URL.Query().Get("path"))
	if !ok || node.Type != string(repo.TypeFile) {
		writeError(w, http.StatusNotFound, "no such file in this snapshot", "")
		return
	}

	m, err := s.Repo.LoadManifest(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such snapshot", err.Error())
		return
	}
	entry, err := s.entryFor(m.ID, node.Path)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such file in this snapshot", err.Error())
		return
	}

	body, err := s.Repo.Objects().Open(r.Context(), entry.Hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	defer body.Close()

	// application/octet-stream and an attachment disposition, always: a file
	// out of a backup is not something to render in the panel's own origin.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEscape(node.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	_, _ = io.Copy(w, body)
}

// entryFor finds one index entry. The tree deliberately does not keep hashes:
// they are useless to a browser and would double the memory a tree costs.
func (s *server) entryFor(snapshot, path string) (repo.Entry, error) {
	ir, closeIdx, err := s.Repo.OpenIndex(snapshot)
	if err != nil {
		return repo.Entry{}, err
	}
	defer closeIdx()
	for {
		e, err := ir.Next()
		if errors.Is(err, io.EOF) {
			return repo.Entry{}, errors.New("web: not in this snapshot")
		}
		if err != nil {
			return repo.Entry{}, err
		}
		if e.Path == path {
			return e, nil
		}
	}
}

// --- settings ---------------------------------------------------------------

func (s *server) handleGetSettings(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, http.StatusOK, map[string]any{
		"settings":         s.Settings.Get(),
		"default_hot":      ops.DefaultHotPatterns(),
		"default_excludes": ops.DefaultExclusions(),
		// The editor needs a number to put back when a rule is switched on
		// again. Sending the built-in defaults keeps that answer in one place
		// rather than hard-coding a second opinion into the script.
		"default_retention": settings.Defaults().Retention,
	})
}

func (s *server) handlePutSettings(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var incoming settings.File
	if err := decodeJSON(r, &incoming); err != nil {
		writeError(w, http.StatusBadRequest, "malformed settings", err.Error())
		return
	}
	saved, err := s.Settings.Update(sess.Identity(), func(f *settings.File) error {
		// Only the editable sections are taken. Instance is reimposed by the
		// store in any case; saying so here as well keeps the intent visible.
		f.Schedule = incoming.Schedule
		f.Retention = incoming.Retention
		f.Exclusions = incoming.Exclusions
		return nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	// The store notifies the scheduler on a channel, and this reply would
	// otherwise be written before the goroutine at the other end had run --
	// so somebody who had just changed the interval would be told the next run
	// worked out from the old one. Recomputing here makes the answer describe
	// the schedule the change produced.
	if s.Scheduler != nil {
		s.Scheduler.Refresh()
	}

	// Answer with what was stored rather than with what was sent, so the tab
	// re-renders from the truth.
	writeJSON(w, http.StatusOK, map[string]any{"settings": saved})
}

// nextRuns is the scheduler's answer in the shape the API uses: UTC, and
// absent rather than zero when nothing is scheduled.
func nextRuns(sch Scheduler) (backup, housekeeping *time.Time) {
	if sch == nil {
		return nil, nil
	}
	b, h := sch.Next()
	if !b.IsZero() {
		t := b.UTC()
		backup = &t
	}
	if !h.IsZero() {
		t := h.UTC()
		housekeeping = &t
	}
	return backup, housekeeping
}

type exclusionPreviewRequest struct {
	Patterns    []string `json:"patterns"`
	UseDefaults bool     `json:"use_defaults"`
}

func (s *server) handleValidateExclusions(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	var req exclusionPreviewRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	preview, err := s.Ops.PreviewExclusions(req.Patterns, req.UseDefaults)
	if err != nil {
		// A pattern that will not compile is the user's typo, not our fault.
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

// handleRetentionPreview answers for the saved policy on GET, and for a
// candidate one on POST.
//
// The candidate half is what makes the rules switchable with a straight face:
// every rule can be turned off on its own, and turning one off is a decision
// about which snapshots stop existing. That number has to be on screen while
// the choice is being made, not discovered at half past four the next morning.
func (s *server) handleRetentionPreview(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	var candidate *settings.Retention
	if r.Method == http.MethodPost {
		var body settings.Retention
		if err := decodeJSON(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, err.Error(), "")
			return
		}
		candidate = &body
	}

	decisions, err := s.Ops.RetentionPreview(candidate)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	var doomed int
	for _, d := range decisions {
		if !d.Keep {
			doomed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"decisions":    decisions,
		"would_forget": doomed,
		"would_keep":   len(decisions) - doomed,
		"snapshots":    len(decisions),
	})
}

// --- jobs -------------------------------------------------------------------

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	writeJSON(w, http.StatusOK, map[string]any{"jobs": s.Jobs.List()})
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	job, logs, err := s.Jobs.Get(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "no such job", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job, "log": logs})
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request, _ *auth.Session) {
	if err := s.Jobs.Cancel(r.PathValue("id")); err != nil {
		writeOperationError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// submit is the shape every mutating endpoint has: hand the work to the runner
// and answer immediately, or say what is in the way.
func (s *server) submit(w http.ResponseWriter, kind jobs.Kind, sess *auth.Session, fn jobs.Func) {
	job, err := s.Jobs.Submit(kind, sess.Identity(), fn)
	if errors.Is(err, jobs.ErrBusy) {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "another job is already running", Reason: "busy", Job: &job,
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

type backupRequest struct {
	Tags     []string `json:"tags"`
	Paranoid bool     `json:"paranoid"`
}

func (s *server) handleRunBackup(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req backupRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	if len(req.Tags) == 0 {
		// So that a snapshot somebody asked for is distinguishable from the
		// hourly ones, and survives retention the same way.
		req.Tags = []string{"manual"}
	}
	s.submit(w, jobs.KindBackup, sess, func(h *jobs.Handle) (any, error) {
		m, err := s.Ops.Backup(h.Context(), h, ops.BackupRequest{
			Tags: req.Tags, Paranoid: req.Paranoid,
		})
		if err != nil {
			s.record(h, state.KindBackup, sess, "", err)
			return nil, err
		}
		s.stats.invalidate()
		s.record(h, state.KindBackup, sess, m.ID, nil)
		return m, nil
	})
}

type restoreRequest struct {
	Snapshot    string   `json:"snapshot"`
	Paths       []string `json:"paths"`
	Target      string   `json:"target"`
	DryRun      bool     `json:"dry_run"`
	PreSnapshot bool     `json:"pre_snapshot"`
}

// maxRestorePaths bounds a selection. The browser sends the minimal cover -- a
// fully ticked directory is one path -- so reaching this means something is
// wrong on the other side, and an honest limit beats an unbounded body.
const maxRestorePaths = 10000

func (s *server) handleRunRestore(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req restoreRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	if req.Snapshot == "" {
		writeError(w, http.StatusBadRequest, "no snapshot given", "")
		return
	}
	if len(req.Paths) > maxRestorePaths {
		writeError(w, http.StatusRequestEntityTooLarge,
			"too many paths selected",
			"select the parent directory instead; a ticked directory counts as one path")
		return
	}
	target := ops.TargetKind(req.Target)
	if target == "" {
		target = ops.TargetScratch
	}

	// The refusal has to happen here, not inside the job: the banner is a
	// reply to this request, and a job that fails a second later would make
	// the tab flash a spinner first.
	if target == ops.TargetInstance {
		if err := s.Ops.CheckRestorable(r.Context()); err != nil {
			writeOperationError(w, err)
			return
		}
	}

	s.submit(w, jobs.KindRestore, sess, func(h *jobs.Handle) (any, error) {
		res, err := s.Ops.Restore(h.Context(), h, ops.RestoreRequest{
			Snapshot: req.Snapshot, Paths: req.Paths, Target: target,
			DryRun: req.DryRun, PreSnapshot: req.PreSnapshot,
		})
		if err != nil {
			s.record(h, state.KindRestore, sess, req.Snapshot, err)
			return nil, err
		}
		s.stats.invalidate()
		if !req.DryRun {
			s.record(h, state.KindRestore, sess, req.Snapshot, nil)
		}
		return res, nil
	})
}

type checkRequest struct {
	ReadData bool `json:"read_data"`
}

func (s *server) handleRunCheck(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req checkRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	s.submit(w, jobs.KindCheck, sess, func(h *jobs.Handle) (any, error) {
		res, err := s.Ops.Housekeeping(h.Context(), h, ops.HousekeepingRequest{
			Check: true, ReadData: req.ReadData,
		})
		s.record(h, state.KindCheck, sess, "", err)
		return res, err
	})
}

type housekeepingRequest struct {
	Forget     bool `json:"forget"`
	Prune      bool `json:"prune"`
	EmptyTrash bool `json:"empty_trash"`
	DryRun     bool `json:"dry_run"`
}

func (s *server) handleRunHousekeeping(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	var req housekeepingRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "malformed request", err.Error())
		return
	}
	s.submit(w, jobs.KindHousekeeping, sess, func(h *jobs.Handle) (any, error) {
		res, err := s.Ops.Housekeeping(h.Context(), h, ops.HousekeepingRequest{
			Check: true, Forget: req.Forget, Prune: req.Prune,
			EmptyTrash: req.EmptyTrash, DryRun: req.DryRun,
		})
		if !req.DryRun {
			s.stats.invalidate()
			s.record(h, state.KindHousekeeping, sess, "", err)
		}
		return res, err
	})
}

func (s *server) handleForget(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id := r.PathValue("id")
	if err := s.Repo.Forget([]string{id}); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	s.Log.Info("snapshot moved to the trash", "snapshot", id, "by", sess.Identity())
	writeJSON(w, http.StatusOK, map[string]any{"forgotten": id})
}

func (s *server) handleUnforget(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	id := r.PathValue("id")
	if err := s.Repo.Unforget(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error(), "")
		return
	}
	s.Log.Info("snapshot taken back out of the trash", "snapshot", id, "by", sess.Identity())
	writeJSON(w, http.StatusOK, map[string]any{"restored": id})
}

// --- the instance ------------------------------------------------------------

// Stop and start run as the caller, not as the service account. The backup
// account deliberately cannot do either; the person looking at the tab can,
// and it is their button.
func (s *server) handleStop(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	s.controlInstance(w, sess, jobs.KindStop, amp.StateStopped,
		func(ctx context.Context, c *amp.Client) error {
			return c.StopApplication(ctx, sess.Control)
		})
}

func (s *server) handleStart(w http.ResponseWriter, r *http.Request, sess *auth.Session) {
	s.controlInstance(w, sess, jobs.KindStart, amp.StateReady,
		func(ctx context.Context, c *amp.Client) error {
			return c.StartApplication(ctx, sess.Control)
		})
}

func (s *server) controlInstance(w http.ResponseWriter, sess *auth.Session, kind jobs.Kind,
	want amp.State, action func(context.Context, *amp.Client) error) {

	if s.UserClient == nil {
		writeError(w, http.StatusNotImplemented, "no panel connection is configured", "")
		return
	}
	s.submit(w, kind, sess, func(h *jobs.Handle) (any, error) {
		client, err := s.UserClient(sess.AMP)
		if err != nil {
			return nil, err
		}
		if err := action(h.Context(), client); err != nil {
			return nil, err
		}
		h.Logf("info", "asked AMP to reach %s; waiting for it", want)
		// Waiting is the point. A stop that has been asked for is not a
		// server that has finished saving, and a restore that starts in
		// between writes underneath a live process.
		if err := client.WaitForState(h.Context(), want, 2*time.Second); err != nil {
			return nil, err
		}
		h.Logf("info", "the instance is %s", want)
		return map[string]any{"state": want.String()}, nil
	})
}

// record files a finished run, so that a restart does not lose it.
func (s *server) record(h *jobs.Handle, kind state.Kind, sess *auth.Session, snapshot string, runErr error) {
	if s.State == nil {
		return
	}
	job := h.Job()
	run := state.Run{
		JobID: job.ID, Kind: kind, StartedAt: job.StartedAt, FinishedAt: s.now(),
		Success: runErr == nil, StartedBy: sess.Identity(), Snapshot: snapshot,
	}
	if runErr != nil {
		run.Error = runErr.Error()
	}
	if err := s.State.Record(run); err != nil {
		s.Log.Warn("could not record the run", "error", err)
	}
}

// --- helpers ----------------------------------------------------------------

func decodeJSON(r *http.Request, out any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	dec := json.NewDecoder(r.Body)
	// An unknown field is almost always a client sending a shape we changed,
	// and reporting it beats silently ignoring half the request.
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// urlEscape encodes a filename for a Content-Disposition header, where a
// modpack jar full of brackets and spaces would otherwise not survive.
func urlEscape(name string) string { return url.PathEscape(name) }

func intParam(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return n
}

// statsCache keeps repository totals, which walk every object and so must not
// run once per page load.
type statsCache struct {
	repo  *repo.Repository
	clock func() time.Time

	mu      sync.Mutex
	value   repo.RepoStats
	at      time.Time
	loading bool
}

const statsMaxAge = time.Minute

func newStatsCache(r *repo.Repository, clock func() time.Time) *statsCache {
	if clock == nil {
		clock = time.Now
	}
	return &statsCache{repo: r, clock: clock}
}

func (c *statsCache) get(ctx context.Context) (repo.RepoStats, time.Duration) {
	c.mu.Lock()
	value, at, loading := c.value, c.at, c.loading
	stale := at.IsZero() || c.clock().Sub(at) > statsMaxAge
	if at.IsZero() && !loading {
		// Nothing to serve yet, so this one caller pays for it.
		c.loading = true
		c.mu.Unlock()
		fresh, err := c.repo.Stats(ctx)
		c.mu.Lock()
		if err == nil {
			c.value, c.at = fresh, c.clock()
		}
		c.loading = false
		value, at = c.value, c.at
		c.mu.Unlock()
		return value, 0
	}
	if stale && !loading {
		c.loading = true
		go c.refresh()
	}
	c.mu.Unlock()
	return value, c.clock().Sub(at)
}

func (c *statsCache) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fresh, err := c.repo.Stats(ctx)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.value, c.at = fresh, c.clock()
	}
	c.loading = false
}

func (c *statsCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = time.Time{}
}
