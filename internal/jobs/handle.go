package jobs

import (
	"context"
	"fmt"
	"time"
)

// Handle is what a running job talks to.
//
// It satisfies the progress interface the operations layer asks for, so a job
// body never knows whether it is being watched by a browser or by nobody.
type Handle struct {
	runner *Runner
	rec    *record
	ctx    context.Context
}

// Context is cancelled when the job is cancelled or the daemon shuts down. A
// job body that ignores it cannot be stopped, so every long loop checks it.
func (h *Handle) Context() context.Context { return h.ctx }

// Stage reports progress.
//
// The job record is updated on every call -- so a page loaded halfway through
// sees the truth -- but an event only goes out when the throttle has elapsed,
// or when the stage changes, or when the stage completes. backup.Run calls
// this once per file from the goroutine doing the work; the throttle is what
// keeps watching a backup from slowing it down.
func (h *Handle) Stage(stage string, done, total int) {
	r := h.runner
	r.mu.Lock()
	defer r.mu.Unlock()

	changed := h.rec.job.Stage != stage
	h.rec.job.Stage = stage
	h.rec.job.Done = done
	h.rec.job.Total = total

	now := r.opts.Clock()
	finished := total > 0 && done >= total
	if !changed && !finished && now.Sub(h.rec.lastEmit) < r.opts.Throttle {
		return
	}
	h.rec.lastEmit = now
	r.publishLocked(Event{Type: "progress", At: now, Job: jobPtr(h.rec.job)})
}

// Logf records a line for this job and sends it to whoever is watching.
//
// This is where the quiesce commentary goes -- when saving went off, how long
// the flush took to confirm -- which until now only reached the journal.
func (h *Handle) Logf(level, format string, args ...any) {
	r := h.runner
	line := LogLine{At: r.opts.Clock(), Level: level, Text: fmt.Sprintf(format, args...)}

	r.mu.Lock()
	defer r.mu.Unlock()
	h.rec.logs = appendCapped(h.rec.logs, r.opts.LogLines, line)
	r.publishLocked(Event{Type: "log", At: line.At, Log: &line})
}

// Job returns the current view of the job being run.
func (h *Handle) Job() Job {
	h.runner.mu.Lock()
	defer h.runner.mu.Unlock()
	return h.rec.job
}

// Deadline, Done, Err and Value make a Handle usable directly as a context,
// which saves every job body from threading two values through.
func (h *Handle) Deadline() (time.Time, bool) { return h.ctx.Deadline() }
func (h *Handle) Done() <-chan struct{}       { return h.ctx.Done() }
func (h *Handle) Err() error                  { return h.ctx.Err() }
func (h *Handle) Value(key any) any           { return h.ctx.Value(key) }
