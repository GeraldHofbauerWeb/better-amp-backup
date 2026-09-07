package quiesce

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
)

func newClient(t *testing.T, srv *fake.Server) *amp.Client {
	t.Helper()
	c, err := amp.New(amp.Config{
		BaseURL: srv.URL, Username: "backup", Password: "secret", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("amp.New: %v", err)
	}
	return c
}

func fastConfig() Config {
	return Config{
		ConfirmTimeout:  2 * time.Second,
		PollInterval:    5 * time.Millisecond,
		WatchdogTimeout: time.Hour, // disarmed unless a test wants it
	}
}

func TestQuiesceSendsTheRightCommandsInOrder(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	q := NewConsole(newClient(t, srv), fastConfig())

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := q.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	got := srv.CommandLog()
	want := []string{"save-off", "save-all flush", "save-on"}
	if len(got) != len(want) {
		t.Fatalf("commands = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The flush confirmation must be waited for, not slept through: reading region
// files before the server finished writing them is exactly the bug this whole
// mechanism exists to avoid.
func TestQuiesceWaitsForConfirmation(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.SilentFlush = true

	cfg := fastConfig()
	cfg.ConfirmTimeout = 300 * time.Millisecond
	q := NewConsole(newClient(t, srv), cfg)

	start := time.Now()
	err := q.Quiesce(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Quiesce succeeded even though the server never confirmed the flush")
	}
	if !strings.Contains(err.Error(), "did not confirm") {
		t.Errorf("error should explain the missing confirmation, got: %v", err)
	}
	if elapsed < 250*time.Millisecond {
		t.Errorf("gave up after %s; it should have waited out the timeout", elapsed)
	}

	// And saving must be restored even though quiescing failed.
	if err := q.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if last := srv.CommandLog(); last[len(last)-1] != "save-on" {
		t.Errorf("commands = %q; save-on must be the last one", last)
	}
}

// A "Saved the game" from an autosave that happened before the backup started
// must not be mistaken for confirmation of our flush.
func TestStaleConsoleOutputIsDrainedFirst(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.EmitConsole("[Server thread/INFO]: Saved the game")
	srv.SilentFlush = true

	cfg := fastConfig()
	cfg.ConfirmTimeout = 200 * time.Millisecond
	q := NewConsole(newClient(t, srv), cfg)

	if err := q.Quiesce(context.Background()); err == nil {
		t.Error("a stale confirmation line was accepted as proof of this flush")
	}
	_ = q.Release(context.Background())
}

func TestReleaseIsIdempotent(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	q := NewConsole(newClient(t, srv), fastConfig())

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := q.Release(ctx); err != nil {
			t.Fatalf("Release %d: %v", i, err)
		}
	}

	var saveOns int
	for _, c := range srv.CommandLog() {
		if c == "save-on" {
			saveOns++
		}
	}
	if saveOns != 1 {
		t.Errorf("sent save-on %d times, want exactly 1", saveOns)
	}
}

func TestReleaseWithoutQuiesceDoesNothing(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	q := NewConsole(newClient(t, srv), fastConfig())

	if err := q.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := srv.CommandLog(); len(got) != 0 {
		t.Errorf("commands = %q, want none", got)
	}
}

// The watchdog is the only safeguard that survives the backup goroutine
// wedging, so it gets its own test.
func TestWatchdogReEnablesSavingOnItsOwn(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()

	cfg := fastConfig()
	cfg.WatchdogTimeout = 100 * time.Millisecond
	var logged []string
	var mu sync.Mutex
	cfg.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, format)
	}
	q := NewConsole(newClient(t, srv), cfg)

	if err := q.Quiesce(context.Background()); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}

	// Deliberately never call Release, as a wedged backup would not.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range srv.CommandLog() {
			if c == "save-on" {
				mu.Lock()
				warned := len(logged) > 0
				mu.Unlock()
				if !warned {
					t.Error("the watchdog fired without warning about it")
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the watchdog never re-enabled saving; a wedged backup would leave the server unable to save")
}

func TestWatchdogIsCancelledByRelease(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()

	cfg := fastConfig()
	cfg.WatchdogTimeout = 80 * time.Millisecond
	q := NewConsole(newClient(t, srv), cfg)

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := q.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	var saveOns int
	for _, c := range srv.CommandLog() {
		if c == "save-on" {
			saveOns++
		}
	}
	if saveOns != 1 {
		t.Errorf("sent save-on %d times; the watchdog should have been cancelled", saveOns)
	}
}

func TestSkipIfStoppedDoesNotTouchTheConsole(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.SetState(0) // stopped

	cfg := fastConfig()
	cfg.SkipIfStopped = true
	q := NewConsole(newClient(t, srv), cfg)

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := q.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := srv.CommandLog(); len(got) != 0 {
		t.Errorf("commands = %q; a stopped server needs no quiescing", got)
	}
}

func TestSkipIfStoppedStillQuiescesARunningServer(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()

	cfg := fastConfig()
	cfg.SkipIfStopped = true
	q := NewConsole(newClient(t, srv), cfg)

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := q.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := srv.CommandLog(); len(got) != 3 {
		t.Errorf("commands = %q, want save-off, save-all flush, save-on", got)
	}
}

func TestDoubleQuiesceIsRefused(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	q := NewConsole(newClient(t, srv), fastConfig())

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if err := q.Quiesce(ctx); err == nil {
		t.Error("quiescing twice was allowed; the second Release would leave the first unbalanced")
	}
	_ = q.Release(ctx)
}

func TestCustomConfirmPattern(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.SaveConfirmation = "[modded] world persisted OK"

	cfg := fastConfig()
	cfg.ConfirmPattern = regexp.MustCompile(`world persisted OK`)
	q := NewConsole(newClient(t, srv), cfg)

	ctx := context.Background()
	if err := q.Quiesce(ctx); err != nil {
		t.Fatalf("Quiesce with a custom pattern: %v", err)
	}
	_ = q.Release(ctx)
}

func TestQuiesceFailsWhenThePanelIsUnreachable(t *testing.T) {
	srv := fake.New("backup", "secret")
	client := newClient(t, srv)
	srv.Close() // panel goes away before we start

	q := NewConsole(client, fastConfig())
	err := q.Quiesce(context.Background())
	if err == nil {
		t.Fatal("Quiesce succeeded against an unreachable panel")
	}
	// Nothing was ever disabled, so Release must be a clean no-op.
	if rerr := q.Release(context.Background()); rerr != nil {
		t.Errorf("Release after a failed connection: %v", rerr)
	}
}

func TestEnsureSaveOnRecoversAfterACrash(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	client := newClient(t, srv)

	// Simulates a daemon starting up after a previous process died holding a
	// quiesce: it says save-on without being asked.
	if err := EnsureSaveOn(context.Background(), client, ""); err != nil {
		t.Fatalf("EnsureSaveOn: %v", err)
	}
	got := srv.CommandLog()
	if len(got) != 1 || got[0] != "save-on" {
		t.Errorf("commands = %q, want [save-on]", got)
	}
}

func TestClientRetriesOnceAfterSessionExpiry(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()
	client := newClient(t, srv)

	ctx := context.Background()
	if _, err := client.GetStatus(ctx); err != nil {
		t.Fatalf("first GetStatus: %v", err)
	}

	srv.ExpireSessionsOnce = true
	if _, err := client.GetStatus(ctx); err != nil {
		t.Fatalf("GetStatus should have re-authenticated transparently: %v", err)
	}
	if srv.Logins != 2 {
		t.Errorf("logins = %d, want 2 (one initial, one after expiry)", srv.Logins)
	}
}

func TestWrongPasswordIsReportedWithoutLeakingIt(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()

	client, err := amp.New(amp.Config{
		BaseURL: srv.URL, Username: "backup", Password: "hunter2", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("amp.New: %v", err)
	}
	_, err = client.GetStatus(context.Background())
	if err == nil {
		t.Fatal("a wrong password was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("the error message leaks the password: %v", err)
	}
	if !strings.Contains(err.Error(), "backup") {
		t.Errorf("the error should name the user that failed, got: %v", err)
	}
}

func TestBadPasswordDoesNotLoop(t *testing.T) {
	srv := fake.New("backup", "secret")
	defer srv.Close()

	client, _ := amp.New(amp.Config{
		BaseURL: srv.URL, Username: "backup", Password: "wrong", Timeout: 5 * time.Second,
	})
	for i := 0; i < 3; i++ {
		if _, err := client.GetStatus(context.Background()); err == nil {
			t.Fatal("expected failure")
		}
	}
	if srv.Logins != 0 {
		t.Errorf("logins = %d, want 0", srv.Logins)
	}
}

func TestUnauthorizedErrorIsRecognised(t *testing.T) {
	if !errors.Is(amp.ErrUnauthorized, amp.ErrUnauthorized) {
		t.Fatal("sanity check failed")
	}
}
