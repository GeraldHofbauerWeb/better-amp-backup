package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/quiesce"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

func consoleQuiescer(t *testing.T, srv *fake.Server) *quiesce.Console {
	t.Helper()
	client, err := amp.New(amp.Config{
		BaseURL: srv.URL, Username: "backup", Password: "secret", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("amp.New: %v", err)
	}
	return quiesce.NewConsole(client, quiesce.Config{
		ConfirmTimeout:  2 * time.Second,
		PollInterval:    5 * time.Millisecond,
		WatchdogTimeout: time.Hour,
		SkipIfStopped:   true,
	})
}

// The whole point of the tiering: a backup of a running server must disable
// saving for the world files and for nothing else.
func TestBackupQuiescesARunningServer(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	srv := fake.New("backup", "secret")
	defer srv.Close()

	opts := baseOptions(f.root)
	opts.Quiescer = consoleQuiescer(t, srv)

	m, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.State != repo.StateComplete {
		t.Errorf("state = %q, warnings %v", m.State, m.Warnings)
	}

	got := srv.CommandLog()
	want := []string{"save-off", "save-all flush", "save-on"}
	if len(got) != len(want) {
		t.Fatalf("console commands = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A stopped instance is already still, so a backup of it must not touch the
// console at all — sending save-off to a stopped server is a no-op at best and
// a confusing log line at worst.
func TestBackupOfAStoppedInstanceSkipsTheConsole(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.SetState(0) // stopped

	opts := baseOptions(f.root)
	opts.Quiescer = consoleQuiescer(t, srv)

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := srv.CommandLog(); len(got) != 0 {
		t.Errorf("console commands = %q, want none", got)
	}
}

// If the server never confirms the flush, the run must fail rather than read a
// world that may be mid-write — and saving must still be turned back on.
func TestBackupFailsRatherThanReadAnUnconfirmedWorld(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	srv := fake.New("backup", "secret")
	defer srv.Close()
	srv.SilentFlush = true

	client, err := amp.New(amp.Config{
		BaseURL: srv.URL, Username: "backup", Password: "secret", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("amp.New: %v", err)
	}
	opts := baseOptions(f.root)
	opts.Quiescer = quiesce.NewConsole(client, quiesce.Config{
		ConfirmTimeout:  200 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		WatchdogTimeout: time.Hour,
	})

	_, err = Run(context.Background(), r, opts)
	if err == nil {
		t.Fatal("the backup proceeded without a confirmed flush")
	}
	if !strings.Contains(err.Error(), "did not confirm") {
		t.Errorf("error should name the missing confirmation, got: %v", err)
	}

	log := srv.CommandLog()
	if len(log) == 0 || log[len(log)-1] != "save-on" {
		t.Errorf("console commands = %q; the server must not be left unable to save", log)
	}
	if snaps, _ := r.ListSnapshots(); len(snaps) != 0 {
		t.Errorf("a failed quiesce still committed %d snapshots", len(snaps))
	}
}

// A second run against a running server should barely disturb it: nothing
// changed, so the hot phase reads nothing, and the console round trip is all
// that happens between save-off and save-on.
func TestIncrementalRunKeepsTheQuiesceWindowShort(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	srv := fake.New("backup", "secret")
	defer srv.Close()

	opts := baseOptions(f.root)
	opts.Quiescer = consoleQuiescer(t, srv)
	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	opts.Quiescer = consoleQuiescer(t, srv)
	second, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if second.Stats.NewObjects != 0 {
		t.Errorf("second run stored %d objects, want 0", second.Stats.NewObjects)
	}
	if second.Stats.UnchangedFiles == 0 {
		t.Error("second run re-read everything; the stat diff did not apply")
	}
}
