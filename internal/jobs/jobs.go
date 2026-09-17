// Package jobs runs the operations that outlive an HTTP request.
//
// One at a time, per daemon. The repository's locks are flocks, which are
// process-level: they stop a second amp-bb process from colliding with this
// one, but two goroutines inside this process would both take the shared write
// lock quite happily. The single-job rule here is therefore the actual
// guarantee that a restore never runs into a backup.
//
// A job reports through a Handle rather than returning at the end, because the
// browser wants to watch. Progress is coalesced on the way out: backup.Run
// calls its progress function synchronously, per file, from the goroutine
// doing the work, and fanning sixty thousand of those into a set of SSE
// encoders would make the backup slower than the thing it replaced.
package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// Kind names what a job does.
type Kind string

const (
	KindBackup       Kind = "backup"
	KindRestore      Kind = "restore"
	KindHousekeeping Kind = "housekeeping"
	KindCheck        Kind = "check"
	KindStop         Kind = "stop"
	KindStart        Kind = "start"
)

// State is where a job is in its life.
type State string

const (
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
	StateCancelled State = "cancelled"
)

// Done reports whether the job has finished, however it finished.
func (s State) Done() bool { return s != StateRunning }

// Job is the public view of one operation.
type Job struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	State     State     `json:"state"`
	StartedBy string    `json:"started_by"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitzero"`

	Stage string `json:"stage,omitempty"`
	Done  int    `json:"done"`
	Total int    `json:"total"`

	Error string `json:"error,omitempty"`
	// Result is whatever the job returned, already encoded. Encoding it once
	// here keeps the runner from having to know any of the result types.
	Result json.RawMessage `json:"result,omitempty"`
	// Cancelling reports that a cancellation was asked for but the job has not
	// unwound yet, so the interface can stop offering the button twice.
	Cancelling bool `json:"cancelling,omitempty"`
}

// LogLine is one line a job emitted.
type LogLine struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Text  string    `json:"text"`
}

// Event is what a watcher receives.
type Event struct {
	Seq  int64     `json:"seq"`
	At   time.Time `json:"at"`
	Type string    `json:"type"` // "job", "progress", "log", "reset"
	Job  *Job      `json:"job,omitempty"`
	Log  *LogLine  `json:"log,omitempty"`
}

// ErrBusy is returned when another job is already running.
var ErrBusy = errors.New("jobs: another job is already running")

// ErrNotFound is returned for an unknown job id.
var ErrNotFound = errors.New("jobs: no such job")

// Func is the body of a job. Whatever it returns is encoded into Job.Result.
type Func func(h *Handle) (result any, err error)

// Options configures a Runner. The zero value is usable.
type Options struct {
	// History is how many finished jobs to keep. Default 20.
	History int
	// LogLines is how many lines to keep per job. Default 2000.
	LogLines int
	// EventBuffer is how many events to keep for replay. Default 1024.
	EventBuffer int
	// Throttle coalesces progress events. Default 250ms.
	Throttle time.Duration
	Clock    func() time.Time
	// NewID generates job ids; the default is the start time plus a counter.
	NewID func(Kind, time.Time, int64) string
}

func (o *Options) applyDefaults() {
	if o.History <= 0 {
		o.History = 20
	}
	if o.LogLines <= 0 {
		o.LogLines = 2000
	}
	if o.EventBuffer <= 0 {
		o.EventBuffer = 1024
	}
	if o.Throttle <= 0 {
		o.Throttle = 250 * time.Millisecond
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.NewID == nil {
		o.NewID = func(k Kind, at time.Time, n int64) string {
			return fmt.Sprintf("%s-%s-%d", k, at.UTC().Format("20060102T150405Z"), n)
		}
	}
}

// Runner owns the one job slot.
type Runner struct {
	opts Options

	mu       sync.Mutex
	counter  int64
	current  *record
	history  []*record
	byID     map[string]*record
	events   []Event
	seq      int64
	watchers []*watcher
	closed   bool

	wg sync.WaitGroup
}

type record struct {
	job  Job
	logs []LogLine
	// cancel unwinds the job's context.
	cancel context.CancelFunc
	// lastEmit throttles progress events.
	lastEmit time.Time
}

type watcher struct {
	ch     chan Event
	closed bool
}

// NewRunner builds a runner.
func NewRunner(opts Options) *Runner {
	opts.applyDefaults()
	return &Runner{opts: opts, byID: map[string]*record{}}
}

// Submit starts fn, if nothing else is running.
func (r *Runner) Submit(kind Kind, by string, fn Func) (Job, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return Job{}, errors.New("jobs: the runner is shutting down")
	}
	if r.current != nil {
		current := r.current.job
		r.mu.Unlock()
		return current, ErrBusy
	}

	now := r.opts.Clock()
	r.counter++
	ctx, cancel := context.WithCancel(context.Background())
	rec := &record{
		job: Job{
			ID:        r.opts.NewID(kind, now, r.counter),
			Kind:      kind,
			State:     StateRunning,
			StartedBy: by,
			StartedAt: now,
		},
		cancel: cancel,
	}
	r.current = rec
	r.byID[rec.job.ID] = rec
	r.publishLocked(Event{Type: "job", Job: jobPtr(rec.job)})
	job := rec.job
	r.wg.Add(1)
	r.mu.Unlock()

	go r.run(ctx, rec, fn)
	return job, nil
}

func (r *Runner) run(ctx context.Context, rec *record, fn Func) {
	defer r.wg.Done()

	h := &Handle{runner: r, rec: rec, ctx: ctx}
	result, err := func() (result any, err error) {
		defer func() {
			// A panic in a job must not take the scheduler and the HTTP server
			// with it. It becomes a failed job, with the panic as the error.
			if p := recover(); p != nil {
				err = fmt.Errorf("jobs: %s panicked: %v", rec.job.Kind, p)
			}
		}()
		return fn(h)
	}()

	r.mu.Lock()
	rec.job.EndedAt = r.opts.Clock()
	switch {
	case err != nil && ctx.Err() != nil:
		// Cancellation wins over whatever error unwinding produced: the
		// operator pressed a button, and "context canceled" tells them nothing
		// they did not already know.
		rec.job.State = StateCancelled
		rec.job.Error = err.Error()
	case err != nil:
		rec.job.State = StateFailed
		rec.job.Error = err.Error()
	case ctx.Err() != nil:
		rec.job.State = StateCancelled
	default:
		rec.job.State = StateSucceeded
	}
	rec.job.Cancelling = false
	if result != nil {
		if raw, encErr := json.Marshal(result); encErr == nil {
			rec.job.Result = raw
		} else {
			rec.logs = appendCapped(rec.logs, r.opts.LogLines, LogLine{
				At: r.opts.Clock(), Level: "warn",
				Text: "the result could not be encoded: " + encErr.Error(),
			})
		}
	}
	rec.cancel()
	r.current = nil
	r.history = append(r.history, rec)
	r.forgetOldLocked()
	r.publishLocked(Event{Type: "job", Job: jobPtr(rec.job)})
	r.mu.Unlock()
}

// forgetOldLocked trims the history, runs with the mutex held.
func (r *Runner) forgetOldLocked() {
	for len(r.history) > r.opts.History {
		dropped := r.history[0]
		r.history = r.history[1:]
		delete(r.byID, dropped.job.ID)
	}
}

// Current returns the running job, if there is one.
func (r *Runner) Current() (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil {
		return Job{}, false
	}
	return r.current.job, true
}

// Get returns a job and its log.
func (r *Runner) Get(id string) (Job, []LogLine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[id]
	if !ok {
		return Job{}, nil, ErrNotFound
	}
	return rec.job, slices.Clone(rec.logs), nil
}

// List returns the running job followed by the remembered ones, newest first.
func (r *Runner) List() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.history)+1)
	if r.current != nil {
		out = append(out, r.current.job)
	}
	for i := len(r.history) - 1; i >= 0; i-- {
		out = append(out, r.history[i].job)
	}
	return out
}

// Cancel asks a running job to unwind.
func (r *Runner) Cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current == nil || r.current.job.ID != id {
		if _, ok := r.byID[id]; ok {
			return errors.New("jobs: that job has already finished")
		}
		return ErrNotFound
	}
	r.current.job.Cancelling = true
	r.publishLocked(Event{Type: "job", Job: jobPtr(r.current.job)})
	r.current.cancel()
	return nil
}

// Subscribe returns a channel of events, replaying everything after since.
//
// Pass a since of 0 for everything still buffered. A subscriber that falls
// behind is dropped rather than allowed to stall the job: the browser
// reconnects with the last sequence number it saw and replays from the ring.
func (r *Runner) Subscribe(since int64) (<-chan Event, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()

	w := &watcher{ch: make(chan Event, 64)}
	backlog := r.eventsAfterLocked(since)
	for _, ev := range backlog {
		select {
		case w.ch <- ev:
		default:
			// The backlog alone is longer than the buffer. Say so rather than
			// delivering a torn prefix of it.
			drainChannel(w.ch)
			w.ch <- Event{Seq: r.seq, At: r.opts.Clock(), Type: "reset"}
		}
	}
	r.watchers = append(r.watchers, w)

	var once sync.Once
	return w.ch, func() {
		once.Do(func() {
			r.mu.Lock()
			r.watchers = slices.DeleteFunc(r.watchers, func(other *watcher) bool { return other == w })
			if !w.closed {
				w.closed = true
				close(w.ch)
			}
			r.mu.Unlock()
		})
	}
}

// LastSeq returns the sequence number of the most recent event.
func (r *Runner) LastSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Shutdown cancels a running job and waits for it to unwind.
func (r *Runner) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	if r.current != nil {
		r.current.job.Cancelling = true
		r.current.cancel()
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}

	r.mu.Lock()
	for _, w := range r.watchers {
		if !w.closed {
			w.closed = true
			close(w.ch)
		}
	}
	r.watchers = nil
	r.mu.Unlock()
	return nil
}

// eventsAfterLocked runs with the mutex held.
func (r *Runner) eventsAfterLocked(since int64) []Event {
	if since <= 0 {
		return slices.Clone(r.events)
	}
	for i, ev := range r.events {
		if ev.Seq > since {
			return slices.Clone(r.events[i:])
		}
	}
	return nil
}

// publishLocked stamps an event, rings it and fans it out. Runs with the mutex
// held; sends are non-blocking by construction.
func (r *Runner) publishLocked(ev Event) {
	r.seq++
	ev.Seq = r.seq
	if ev.At.IsZero() {
		ev.At = r.opts.Clock()
	}
	r.events = appendCapped(r.events, r.opts.EventBuffer, ev)

	live := r.watchers[:0]
	for _, w := range r.watchers {
		select {
		case w.ch <- ev:
			live = append(live, w)
		default:
			// This watcher is not keeping up. Dropping it is the only option
			// that does not let a slow reader hold up a restore.
			if !w.closed {
				w.closed = true
				close(w.ch)
			}
		}
	}
	r.watchers = live
}

func jobPtr(j Job) *Job { return &j }

func appendCapped[T any](buf []T, max int, v T) []T {
	buf = append(buf, v)
	if len(buf) > max {
		buf = buf[len(buf)-max:]
	}
	return buf
}

func drainChannel[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
