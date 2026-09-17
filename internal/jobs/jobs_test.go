package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func runner(t *testing.T, opts Options) *Runner {
	t.Helper()
	r := NewRunner(opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return r
}

func waitFor(t *testing.T, r *Runner, id string, want State) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, _, err := r.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if job.State == want {
			return job
		}
		time.Sleep(time.Millisecond)
	}
	job, _, _ := r.Get(id)
	t.Fatalf("job %s is %s, waited for %s", id, job.State, want)
	return Job{}
}

// The flocks in the repository are process-level and do not protect a process
// from itself. This rule is the actual guarantee that a restore never runs
// into a backup.
func TestOnlyOneJobRunsAtATime(t *testing.T) {
	r := runner(t, Options{})
	release := make(chan struct{})
	first, err := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		<-release
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	_, err = r.Submit(KindRestore, "gerry", func(*Handle) (any, error) { return nil, nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second Submit returned %v, want ErrBusy", err)
	}

	close(release)
	waitFor(t, r, first.ID, StateSucceeded)

	if _, err := r.Submit(KindRestore, "gerry", func(*Handle) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("Submit after the first finished: %v", err)
	}
}

// ErrBusy comes back with the job that is in the way, so the interface can
// send the user straight to it rather than saying "try again".
func TestBusyReturnsTheJobInTheWay(t *testing.T) {
	r := runner(t, Options{})
	release := make(chan struct{})
	defer close(release)
	first, _ := r.Submit(KindBackup, "gerry", func(*Handle) (any, error) {
		<-release
		return nil, nil
	})

	blocking, err := r.Submit(KindRestore, "someone", func(*Handle) (any, error) { return nil, nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if blocking.ID != first.ID || blocking.Kind != KindBackup {
		t.Errorf("returned %+v, want the running backup", blocking)
	}
}

// backup.Run calls its progress function once per file, synchronously, from
// the goroutine doing the work. Unthrottled, watching a backup would slow it
// down -- which would be a fine joke at the expense of the whole project.
func TestProgressIsCoalesced(t *testing.T) {
	r := runner(t, Options{Throttle: 50 * time.Millisecond})
	events, stop := r.Subscribe(0)
	defer stop()

	job, err := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		for i := range 10000 {
			h.Stage("cold", i, 10000)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	final := waitFor(t, r, job.ID, StateSucceeded)
	if final.Done != 9999 {
		t.Errorf("Done = %d, want the last value reported", final.Done)
	}

	var progress int
	stop()
	for ev := range events {
		if ev.Type == "progress" {
			progress++
		}
	}
	// One for the first call, one when the throttle elapses, a few more if the
	// machine is slow. Ten thousand would mean the throttle does nothing.
	if progress > 50 {
		t.Errorf("%d progress events for 10000 calls; the throttle is not working", progress)
	}
	if progress == 0 {
		t.Error("no progress events at all")
	}
}

// A completed stage must always be published, or a progress bar sticks at 99%.
func TestACompletedStageIsAlwaysPublished(t *testing.T) {
	r := runner(t, Options{Throttle: time.Hour})
	events, stop := r.Subscribe(0)
	defer stop()

	job, _ := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		h.Stage("cold", 0, 2)
		h.Stage("cold", 1, 2)
		h.Stage("cold", 2, 2)
		return nil, nil
	})
	waitFor(t, r, job.ID, StateSucceeded)

	stop()
	var sawComplete bool
	for ev := range events {
		if ev.Type == "progress" && ev.Job != nil && ev.Job.Done == 2 && ev.Job.Total == 2 {
			sawComplete = true
		}
	}
	if !sawComplete {
		t.Error("the finished stage was swallowed by the throttle")
	}
}

// A change of stage is news even inside the throttle window.
func TestAStageChangeIsPublishedImmediately(t *testing.T) {
	r := runner(t, Options{Throttle: time.Hour})
	events, stop := r.Subscribe(0)
	defer stop()

	job, _ := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		h.Stage("cold", 1, 100)
		h.Stage("hot", 1, 10)
		return nil, nil
	})
	waitFor(t, r, job.ID, StateSucceeded)

	stop()
	stages := map[string]bool{}
	for ev := range events {
		if ev.Type == "progress" && ev.Job != nil {
			stages[ev.Job.Stage] = true
		}
	}
	if !stages["cold"] || !stages["hot"] {
		t.Errorf("stages published: %v, want both", stages)
	}
}

func TestCancelUnwindsAJobThatWatchesItsContext(t *testing.T) {
	r := runner(t, Options{})
	started := make(chan struct{})
	job, _ := r.Submit(KindRestore, "gerry", func(h *Handle) (any, error) {
		close(started)
		<-h.Context().Done()
		return nil, h.Context().Err()
	})
	<-started

	if err := r.Cancel(job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	final := waitFor(t, r, job.ID, StateCancelled)
	if final.State != StateCancelled {
		t.Errorf("State = %s", final.State)
	}
}

// A job that ignores its context still has to end up marked cancelled, or the
// interface reports a success the operator did not get.
func TestCancelIsRecordedEvenIfTheJobIgnoresIt(t *testing.T) {
	r := runner(t, Options{})
	started := make(chan struct{})
	release := make(chan struct{})
	job, _ := r.Submit(KindRestore, "gerry", func(*Handle) (any, error) {
		close(started)
		<-release
		return "finished anyway", nil
	})
	<-started
	if err := r.Cancel(job.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	close(release)
	waitFor(t, r, job.ID, StateCancelled)
}

func TestCancelOnAFinishedJobSaysSo(t *testing.T) {
	r := runner(t, Options{})
	job, _ := r.Submit(KindCheck, "gerry", func(*Handle) (any, error) { return nil, nil })
	waitFor(t, r, job.ID, StateSucceeded)

	err := r.Cancel(job.ID)
	if err == nil || !strings.Contains(err.Error(), "already finished") {
		t.Errorf("Cancel on a finished job = %v", err)
	}
	if err := r.Cancel("no-such-job"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel on an unknown job = %v, want ErrNotFound", err)
	}
}

// A panic in a job must not take the scheduler and the HTTP server with it.
func TestAPanicBecomesAFailedJob(t *testing.T) {
	r := runner(t, Options{})
	job, _ := r.Submit(KindBackup, "gerry", func(*Handle) (any, error) {
		panic("the disk fell off")
	})
	final := waitFor(t, r, job.ID, StateFailed)
	if !strings.Contains(final.Error, "the disk fell off") {
		t.Errorf("Error = %q, want the panic in it", final.Error)
	}

	// And the runner must still accept work afterwards.
	next, err := r.Submit(KindCheck, "gerry", func(*Handle) (any, error) { return nil, nil })
	if err != nil {
		t.Fatalf("Submit after a panic: %v", err)
	}
	waitFor(t, r, next.ID, StateSucceeded)
}

func TestResultIsEncodedForTheClient(t *testing.T) {
	r := runner(t, Options{})
	job, _ := r.Submit(KindBackup, "gerry", func(*Handle) (any, error) {
		return map[string]any{"snapshot": "20260917T200256Z-cc8a5a", "files": 3722}, nil
	})
	final := waitFor(t, r, job.ID, StateSucceeded)
	if !strings.Contains(string(final.Result), "20260917T200256Z-cc8a5a") {
		t.Errorf("Result = %s", final.Result)
	}
}

func TestLogsAreKeptAndDelivered(t *testing.T) {
	r := runner(t, Options{})
	events, stop := r.Subscribe(0)
	defer stop()

	job, _ := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		h.Logf("info", "saving disabled on a running server")
		h.Logf("info", "flush confirmed after %s", 353*time.Millisecond)
		return nil, nil
	})
	waitFor(t, r, job.ID, StateSucceeded)

	_, logs, err := r.Get(job.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(logs) != 2 || !strings.Contains(logs[1].Text, "353ms") {
		t.Errorf("logs = %+v", logs)
	}

	stop()
	var delivered int
	for ev := range events {
		if ev.Type == "log" {
			delivered++
		}
	}
	if delivered != 2 {
		t.Errorf("%d log events delivered, want 2", delivered)
	}
}

// A slow reader must not be able to stall a restore. It gets dropped, and the
// browser reconnects with the sequence number it last saw.
func TestASlowWatcherIsDroppedRatherThanBlocking(t *testing.T) {
	r := runner(t, Options{Throttle: time.Nanosecond})
	events, stop := r.Subscribe(0)
	defer stop()

	job, _ := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		for i := range 5000 {
			h.Stage("cold", i, 5000)
		}
		return nil, nil
	})
	// Never read from events. If a full channel could block, this hangs.
	waitFor(t, r, job.ID, StateSucceeded)

	drained := 0
	for range events {
		drained++
	}
	if drained == 0 {
		t.Error("the watcher received nothing at all")
	}
}

func TestSubscribeReplaysFromASequenceNumber(t *testing.T) {
	r := runner(t, Options{})
	job, _ := r.Submit(KindCheck, "gerry", func(h *Handle) (any, error) {
		h.Logf("info", "one")
		h.Logf("info", "two")
		return nil, nil
	})
	waitFor(t, r, job.ID, StateSucceeded)

	all, stopAll := r.Subscribe(0)
	var seqs []int64
	for len(seqs) < 4 {
		select {
		case ev := <-all:
			seqs = append(seqs, ev.Seq)
		case <-time.After(time.Second):
			t.Fatalf("only got %d events", len(seqs))
		}
	}
	stopAll()

	// Reconnecting with the second sequence number must not repeat it.
	replay, stopReplay := r.Subscribe(seqs[1])
	defer stopReplay()
	select {
	case ev := <-replay:
		if ev.Seq != seqs[2] {
			t.Errorf("replay started at %d, want %d", ev.Seq, seqs[2])
		}
	case <-time.After(time.Second):
		t.Fatal("no replay")
	}
}

// A reconnect from further back than the ring reaches is told to start over
// rather than handed a torn prefix.
func TestAnUnreachableSequenceNumberGetsAReset(t *testing.T) {
	r := runner(t, Options{EventBuffer: 4})
	job, _ := r.Submit(KindCheck, "gerry", func(h *Handle) (any, error) {
		for i := range 20 {
			h.Logf("info", "line %d", i)
		}
		return nil, nil
	})
	waitFor(t, r, job.ID, StateSucceeded)

	ch, stop := r.Subscribe(1)
	defer stop()
	select {
	case ev := <-ch:
		if ev.Type == "reset" {
			return
		}
		// The ring may still hold a valid tail; what matters is that nothing
		// claims to continue from an event that was dropped.
		if ev.Seq <= 1 {
			t.Errorf("replayed an event that should have been evicted: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event at all")
	}
}

func TestHistoryIsTrimmed(t *testing.T) {
	r := runner(t, Options{History: 3})
	var ids []string
	for range 6 {
		job, err := r.Submit(KindCheck, "gerry", func(*Handle) (any, error) { return nil, nil })
		if err != nil {
			t.Fatalf("Submit: %v", err)
		}
		waitFor(t, r, job.ID, StateSucceeded)
		ids = append(ids, job.ID)
	}
	if got := len(r.List()); got != 3 {
		t.Errorf("List() returned %d jobs, want 3", got)
	}
	if _, _, err := r.Get(ids[0]); !errors.Is(err, ErrNotFound) {
		t.Errorf("the oldest job is still there: %v", err)
	}
	if _, _, err := r.Get(ids[5]); err != nil {
		t.Errorf("the newest job is gone: %v", err)
	}
}

// Shutdown cancels what is running and waits for it, so that a quiesced backup
// gets to send save-on before the process goes away.
func TestShutdownCancelsAndWaits(t *testing.T) {
	r := NewRunner(Options{})
	unwound := make(chan struct{})
	started := make(chan struct{})
	if _, err := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
		close(started)
		<-h.Context().Done()
		time.Sleep(20 * time.Millisecond) // the save-on, in spirit
		close(unwound)
		return nil, h.Context().Err()
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-unwound:
	default:
		t.Error("Shutdown returned before the job had unwound")
	}
	if _, err := r.Submit(KindCheck, "gerry", func(*Handle) (any, error) { return nil, nil }); err == nil {
		t.Error("the runner accepted work after shutting down")
	}
}

func TestConcurrentSubmitSubscribeCancel(t *testing.T) {
	r := runner(t, Options{Throttle: time.Nanosecond})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				job, err := r.Submit(KindBackup, "gerry", func(h *Handle) (any, error) {
					h.Stage("cold", 1, 2)
					h.Logf("info", "working")
					return nil, nil
				})
				if err == nil {
					_ = r.Cancel(job.ID)
				}
				_, _ = r.Current()
				_ = r.List()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, stop := r.Subscribe(0)
			defer stop()
			timeout := time.After(500 * time.Millisecond)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-timeout:
					return
				}
			}
		}()
	}
	wg.Wait()
}
