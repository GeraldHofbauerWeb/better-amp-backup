package schedule

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
)

type harness struct {
	scheduler *Scheduler
	settings  *settings.Store
	state     *state.Store
	jobs      *jobs.Runner
	now       time.Time
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func newHarness(t *testing.T, tune func(*settings.File)) *harness {
	t.Helper()
	cfg := settings.Defaults()
	cfg.Instance = settings.Instance{Name: "Demo", Root: "/tmp"}
	cfg.Schedule.Jitter = "0s"
	if tune != nil {
		tune(&cfg)
	}
	store, err := settings.NewMemory(cfg)
	if err != nil {
		t.Fatalf("settings: %v", err)
	}
	st, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	h := &harness{
		settings: store, state: st,
		jobs: jobs.NewRunner(jobs.Options{}),
		now:  time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC),
	}
	h.jobs = jobs.NewRunner(jobs.Options{Clock: func() time.Time { return h.now }})
	h.scheduler = New(store, st, h.jobs, nil, nil, func() time.Time { return h.now })
	h.scheduler.jitter = func(time.Duration) time.Duration { return 0 }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.jobs.Shutdown(ctx)
	})
	return h
}

// The first backup after a start waits out the grace period, so that a host
// reboot does not back up an instance AMP is still bringing up.
func TestTheFirstRunWaitsForTheGracePeriod(t *testing.T) {
	h := newHarness(t, func(f *settings.File) {
		f.Schedule.Every = "1h"
		f.Schedule.StartupGrace = "2m"
	})
	backup, _ := h.scheduler.Next()
	if got := backup.Sub(h.now); got < 2*time.Minute {
		t.Errorf("the first backup is due in %s, want at least the two-minute grace", got)
	}
}

// Counting from the last run rather than from now is what stops a restart loop
// becoming a backup loop.
func TestTheNextRunCountsFromTheLastOne(t *testing.T) {
	h := newHarness(t, func(f *settings.File) { f.Schedule.Every = "1h" })
	lastRun := h.now.Add(-50 * time.Minute)
	if err := h.state.Record(state.Run{
		Kind: state.KindBackup, StartedAt: lastRun, FinishedAt: lastRun, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	h.scheduler.recompute()

	backup, _ := h.scheduler.Next()
	if want := lastRun.Add(time.Hour); !backup.Equal(want) {
		t.Errorf("next backup at %s, want %s (ten minutes from now, not an hour)", backup, want)
	}
}

// The bug this exists to prevent: a job is asynchronous, so at the moment of
// firing the state still holds the previous run. Recomputing from the state
// alone would find a backup due immediately and fire again, once per loop.
func TestFiringDoesNotImmediatelyMakeAnotherRunDue(t *testing.T) {
	h := newHarness(t, func(f *settings.File) {
		f.Schedule.Every = "1h"
		f.Schedule.StartupGrace = "0s"
	})
	// ops is nil here: the job that gets submitted will fail, which the runner
	// turns into a failed job rather than a crash. What is under test is when
	// the next one becomes due, not what the run did.
	h.advance(2 * time.Hour)
	h.scheduler.recompute()
	h.scheduler.fireDue(context.Background())

	// Whatever the state says, the next one must be an hour out.
	h.scheduler.recompute()
	backup, _ := h.scheduler.Next()
	if got := backup.Sub(h.now); got < 59*time.Minute {
		t.Fatalf("the next backup is due in %s; it would fire again at once", got)
	}
}

func TestADisabledScheduleIsNotDue(t *testing.T) {
	h := newHarness(t, func(f *settings.File) {
		f.Schedule.Enabled = false
		f.Schedule.Housekeeping.Enabled = false
	})
	backup, house := h.scheduler.Next()
	if !backup.IsZero() || !house.IsZero() {
		t.Errorf("next = %s / %s, want neither scheduled", backup, house)
	}
}

// Calendar arithmetic, not "add twenty-four hours": 04:30 has to stay 04:30
// across a daylight-saving change, and that is a setting somebody really picks.
func TestHousekeepingHoldsItsTimeAcrossADaylightSavingChange(t *testing.T) {
	vienna, err := time.LoadLocation("Europe/Vienna")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	h := settings.Housekeeping{Enabled: true, At: "04:30", TZ: "Europe/Vienna", Check: true}

	// The night the clocks go back in 2026.
	before := time.Date(2026, 10, 25, 2, 0, 0, 0, vienna)
	due, err := nextHousekeeping(h, before)
	if err != nil {
		t.Fatalf("nextHousekeeping: %v", err)
	}
	local := due.In(vienna)
	if local.Hour() != 4 || local.Minute() != 30 {
		t.Errorf("due at %s local, want 04:30", local.Format("15:04"))
	}

	// And the following day, after the change.
	after, err := nextHousekeeping(h, local.Add(time.Minute))
	if err != nil {
		t.Fatalf("nextHousekeeping: %v", err)
	}
	next := after.In(vienna)
	if next.Hour() != 4 || next.Minute() != 30 {
		t.Errorf("the following run is at %s, want 04:30", next.Format("15:04"))
	}
	if !next.After(local) {
		t.Errorf("the following run is not in the future: %s then %s", local, next)
	}
}

func TestHousekeepingIsAlwaysInTheFuture(t *testing.T) {
	h := settings.Housekeeping{Enabled: true, At: "04:30", Check: true}
	now := time.Date(2026, 9, 18, 4, 30, 0, 0, time.Local)
	due, err := nextHousekeeping(h, now)
	if err != nil {
		t.Fatal(err)
	}
	if !due.After(now) {
		t.Errorf("due %s is not after %s; it would fire in a loop", due, now)
	}
}

// Busy is not a failure, but an unexplained gap in the snapshot list is.
func TestABusyRunnerProducesASkipRatherThanAFailure(t *testing.T) {
	h := newHarness(t, func(f *settings.File) {
		f.Schedule.Every = "1h"
		f.Schedule.StartupGrace = "0s"
	})

	release := make(chan struct{})
	defer close(release)
	if _, err := h.jobs.Submit(jobs.KindRestore, "gerry", func(*jobs.Handle) (any, error) {
		<-release
		return nil, nil
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	h.advance(2 * time.Hour)
	h.scheduler.recompute()
	h.scheduler.fireDue(context.Background())

	skip := h.state.Get().LastSkip
	if skip == nil {
		t.Fatal("no skip was recorded")
	}
	if skip.Kind != state.KindBackup {
		t.Errorf("Kind = %s", skip.Kind)
	}
	if skip.Reason == "" {
		t.Error("the skip has no reason; the gap would be unexplainable")
	}
}

// A settings change has to take effect without waiting out the old interval.
func TestAChangedIntervalIsPickedUp(t *testing.T) {
	h := newHarness(t, func(f *settings.File) {
		f.Schedule.Every = "6h"
		f.Schedule.StartupGrace = "0s"
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.scheduler.Run(ctx) }()

	before, _ := h.scheduler.Next()
	if _, err := h.settings.Update("gerry", func(f *settings.File) error {
		f.Schedule.Every = "15m"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if after, _ := h.scheduler.Next(); after.Before(before) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the scheduler did not react to the new interval")
}
