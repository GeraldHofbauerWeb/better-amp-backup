// Package schedule owns the backup and housekeeping cadence.
//
// The daemon runs as the amp user. It cannot rewrite a systemd timer or reload
// systemd, and giving it the ability to would undo the reason it runs
// unprivileged in the first place. So it keeps its own clock: the cadence
// lives in settings, the last fire in state, and the combination is what the
// timer's Persistent=true used to provide -- a restart neither loses an hour
// nor fires immediately.
package schedule

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/ops"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
)

// Scheduler fires backups and the housekeeping pass.
type Scheduler struct {
	settings *settings.Store
	state    *state.Store
	jobs     *jobs.Runner
	ops      *ops.Runner
	log      *slog.Logger
	clock    func() time.Time
	// jitter is a seam: the tests need a deterministic one.
	jitter func(time.Duration) time.Duration
	// LastSnapshotAt reports when the newest snapshot in the repository was
	// started, whoever produced it. Without it the first run after a restart
	// is counted from the restart, so every restart postpones the backup by a
	// full interval -- and right after migrating off the systemd timers, when
	// the daemon has no history of its own at all, that is every time.
	LastSnapshotAt func() time.Time

	started time.Time

	// mu guards next and fired. Next is read by the status page on whatever
	// goroutine the request landed on, while Run writes it.
	mu sync.RWMutex
	// fired records when this process last started each kind of run.
	//
	// It is not the same as the state file, and the difference matters: a job
	// is asynchronous, so at the moment of firing the state still holds the
	// previous run. Recomputing from the state alone would find the last
	// backup an hour old, decide one was due immediately, and fire again --
	// once per loop, for ever.
	fired struct {
		backup time.Time
	}

	next struct {
		backup       time.Time
		housekeeping time.Time
	}
	// changed carries recomputed times out to whoever is displaying them.
	changed chan struct{}
}

// New builds a scheduler.
func New(st *settings.Store, sr *state.Store, jr *jobs.Runner, op *ops.Runner,
	log *slog.Logger, clock func() time.Time) *Scheduler {

	if log == nil {
		log = slog.Default()
	}
	if clock == nil {
		clock = time.Now
	}
	s := &Scheduler{
		settings: st, state: sr, jobs: jr, ops: op,
		log: log, clock: clock,
		jitter:  func(window time.Duration) time.Duration { return randomWithin(window) },
		changed: make(chan struct{}, 1),
	}
	s.started = clock()
	s.recompute()
	return s
}

// Next reports when each kind of run is due. A zero time means "not
// scheduled", which is what the status page shows for a disabled schedule.
func (s *Scheduler) Next() (backup, housekeeping time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.next.backup, s.next.housekeeping
}

// Refresh recomputes the next times now, rather than when the run loop next
// wakes up.
//
// Saving the settings goes through the store, which notifies this scheduler on
// a channel -- and the reply to that save is written before the goroutine on
// the other end of it has necessarily run. Whoever just changed the interval
// would therefore be told the old next run, and would keep being told it until
// the next poll. It is the same computation the loop does, under the same
// lock, and doing it twice changes nothing.
func (s *Scheduler) Refresh() { s.recompute() }

// Run blocks until ctx is done.
func (s *Scheduler) Run(ctx context.Context) error {
	updates, unsubscribe := s.settings.Subscribe()
	defer unsubscribe()

	// Between New and here somebody may already have changed the settings, and
	// that notification went nowhere. Recomputing once now closes the window.
	s.recompute()

	// One timer, recomputed after every fire and on every settings change.
	// Never a ticker: the interval is editable while this is running, and a
	// ticker would keep the old one until it happened to be replaced.
	timer := time.NewTimer(s.untilNext())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-updates:
			s.recompute()
			resetTimer(timer, s.untilNext())

		case <-timer.C:
			s.fireDue(ctx)
			s.recompute()
			resetTimer(timer, s.untilNext())
		}
	}
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// untilNext is how long to sleep. It is bounded above so that a clock jump or
// a long-disabled schedule still wakes up occasionally and re-reads its own
// configuration.
func (s *Scheduler) untilNext() time.Duration {
	const maxSleep = 5 * time.Minute
	now := s.clock()

	soonest := time.Time{}
	for _, at := range []time.Time{s.next.backup, s.next.housekeeping} {
		if at.IsZero() {
			continue
		}
		if soonest.IsZero() || at.Before(soonest) {
			soonest = at
		}
	}
	if soonest.IsZero() {
		return maxSleep
	}
	d := soonest.Sub(now)
	if d < 0 {
		d = 0
	}
	if d > maxSleep {
		d = maxSleep
	}
	return d
}

// recompute works out when each kind of run is next due.
func (s *Scheduler) recompute() {
	s.mu.Lock()
	defer s.mu.Unlock()

	cfg := s.settings.Get()
	st := s.stateOrEmpty()
	now := s.clock()

	s.next.backup = time.Time{}
	if cfg.Schedule.Enabled {
		every, err := cfg.Schedule.Interval()
		if err != nil {
			s.log.Error("the backup interval is unusable; backups are not scheduled", "error", err)
		} else {
			// Counting from the last run rather than from now is what stops a
			// restart loop turning into a backup loop.
			// The base is the last run, whichever source knows about it --
			// counting from the daemon's own start instead would make a
			// restart loop into a backup loop, which is exactly what the
			// systemd timer's Persistent=true was there to prevent.
			base := time.Time{}
			if st.LastBackup != nil {
				base = st.LastBackup.StartedAt
			}
			if s.LastSnapshotAt != nil {
				if at := s.LastSnapshotAt(); at.After(base) {
					base = at
				}
			}
			if s.fired.backup.After(base) {
				base = s.fired.backup
			}
			if base.IsZero() {
				base = s.started
			}
			due := base.Add(every)

			// The grace period is a floor, not a base: a host reboot must not
			// back up an instance AMP is still bringing up, but it must not
			// push a long-overdue backup further away either.
			grace, err := cfg.Schedule.Grace()
			if err == nil {
				if floor := s.started.Add(grace); due.Before(floor) {
					due = floor
				}
			}
			if due.Before(now) {
				due = now
			}
			s.next.backup = due
		}
	}

	s.next.housekeeping = time.Time{}
	if cfg.Schedule.Housekeeping.Enabled {
		due, err := nextHousekeeping(cfg.Schedule.Housekeeping, now)
		if err != nil {
			s.log.Error("the housekeeping time is unusable; it is not scheduled", "error", err)
		} else {
			s.next.housekeeping = due
		}
	}
}

func (s *Scheduler) stateOrEmpty() state.State {
	if s.state == nil {
		return state.State{}
	}
	return s.state.Get()
}

// nextHousekeeping is calendar arithmetic, deliberately.
//
// Adding twenty-four hours drifts by an hour twice a year, and 04:30 in a
// European time zone is exactly the setting somebody picks. The zone is part
// of the schedule for the same reason.
//
// The result is always strictly after now, which is also what stops a
// just-fired pass from being found due again on the next loop -- the backup
// side needs an explicit guard for that, this one does not.
func nextHousekeeping(h settings.Housekeeping, now time.Time) (time.Time, error) {
	loc, err := h.Location()
	if err != nil {
		return time.Time{}, err
	}
	hour, minute, err := h.TimeOfDay()
	if err != nil {
		return time.Time{}, err
	}

	local := now.In(loc)
	due := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !due.After(local) {
		due = due.AddDate(0, 0, 1)
	}
	return due, nil
}

// fireDue runs whatever is due now.
func (s *Scheduler) fireDue(ctx context.Context) {
	s.mu.Lock()
	now := s.clock()
	dueBackup := !s.next.backup.IsZero() && !s.next.backup.After(now)
	dueHouse := !s.next.housekeeping.IsZero() && !s.next.housekeeping.After(now)
	if dueBackup {
		s.fired.backup = now
	}
	s.mu.Unlock()

	if dueBackup {
		s.fire(ctx, jobs.KindBackup, state.KindBackup, func(h *jobs.Handle) (any, error) {
			return s.ops.Backup(h.Context(), h, ops.BackupRequest{Tags: []string{"scheduled"}})
		})
	}
	if dueHouse {
		cfg := s.settings.Get().Schedule.Housekeeping
		s.fire(ctx, jobs.KindHousekeeping, state.KindHousekeeping, func(h *jobs.Handle) (any, error) {
			return s.ops.Housekeeping(h.Context(), h, ops.HousekeepingRequest{
				Check: cfg.Check, Forget: cfg.Forget,
				Prune: cfg.Prune, EmptyTrash: cfg.EmptyTrash,
			})
		})
	}
}

func (s *Scheduler) fire(ctx context.Context, kind jobs.Kind, recorded state.Kind, fn jobs.Func) {
	if window, err := s.settings.Get().Schedule.JitterWindow(); err == nil && window > 0 && kind == jobs.KindBackup {
		// AMP's own backup runs on the hour. Landing beside it rather than on
		// it is the whole reason this delay exists.
		delay := s.jitter(window)
		s.log.Info("waiting before the scheduled run", "kind", kind, "delay", delay.Round(time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}

	started := s.clock()
	_, err := s.jobs.Submit(kind, "scheduler", func(h *jobs.Handle) (any, error) {
		result, err := fn(h)
		s.record(h, recorded, started, err)
		return result, err
	})
	if err == nil {
		return
	}

	// Busy is not a failure. A backup that was due while a three-hour restore
	// ran is not worth taking afterwards -- the next one is an hour away --
	// but a gap in the snapshot list that nobody can explain afterwards is.
	reason := err.Error()
	if current, ok := s.jobs.Current(); ok {
		reason = "a " + string(current.Kind) + " was running"
	}
	s.log.Warn("scheduled run skipped", "kind", kind, "reason", reason)
	if s.state != nil {
		if err := s.state.RecordSkip(state.Skip{At: started, Kind: recorded, Reason: reason}); err != nil {
			s.log.Warn("could not record the skip", "error", err)
		}
	}
}

func (s *Scheduler) record(h *jobs.Handle, kind state.Kind, started time.Time, runErr error) {
	if s.state == nil {
		return
	}
	run := state.Run{
		JobID: h.Job().ID, Kind: kind, StartedAt: started, FinishedAt: s.clock(),
		Success: runErr == nil, StartedBy: "scheduler",
	}
	if runErr != nil {
		run.Error = runErr.Error()
	}
	if err := s.state.Record(run); err != nil {
		s.log.Warn("could not record the run", "error", err)
	}
}

func randomWithin(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(window)))
}
