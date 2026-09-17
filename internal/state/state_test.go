package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// This is the property the systemd timer had for free with Persistent=true:
// after a restart the daemon still knows when the last backup started, so it
// neither loses an hour nor fires immediately.
func TestTheLastRunSurvivesARestart(t *testing.T) {
	store, path := openStore(t)
	started := time.Date(2026, 9, 17, 20, 2, 56, 0, time.UTC)
	if err := store.Record(Run{
		JobID: "j1", Kind: KindBackup, StartedAt: started,
		FinishedAt: started.Add(4 * time.Second), Success: true,
		StartedBy: "scheduler", Snapshot: "20260917T200256Z-cc8a5a",
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := reopened.Get().LastBackup
	if got == nil {
		t.Fatal("the last backup was forgotten")
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, started)
	}
	if got.Duration() != 4*time.Second {
		t.Errorf("Duration = %v, want 4s", got.Duration())
	}
	if got.StartedBy != "scheduler" {
		t.Errorf("StartedBy = %q", got.StartedBy)
	}
}

func TestEachKindIsRecordedSeparately(t *testing.T) {
	store, _ := openStore(t)
	for _, kind := range []Kind{KindBackup, KindRestore, KindHousekeeping, KindCheck} {
		if err := store.Record(Run{Kind: kind, JobID: string(kind), Success: true}); err != nil {
			t.Fatalf("Record(%s): %v", kind, err)
		}
	}
	st := store.Get()
	for name, run := range map[string]*Run{
		"backup": st.LastBackup, "restore": st.LastRestore,
		"housekeeping": st.LastHousekeep, "check": st.LastCheck,
	} {
		if run == nil {
			t.Errorf("%s was not recorded", name)
		}
	}
}

// A skipped run has to leave a trace, or the gap in the snapshot list is
// unexplainable afterwards.
func TestASkipIsRemembered(t *testing.T) {
	store, path := openStore(t)
	at := time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)
	if err := store.RecordSkip(Skip{At: at, Kind: KindBackup, Reason: "a restore was running"}); err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}
	reopened, _ := Open(path)
	got := reopened.Get().LastSkip
	if got == nil || got.Reason != "a restore was running" {
		t.Errorf("LastSkip = %+v", got)
	}
}

// Refusing to start a backup daemon because its bookkeeping is corrupt would
// turn a cosmetic problem into a missing backup.
func TestACorruptStateFileStartsEmptyRatherThanFailing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json at all"), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open refused to start: %v", err)
	}
	if store.Get().LastBackup != nil {
		t.Error("something was read out of a corrupt file")
	}
	// And it must be writable again afterwards.
	if err := store.Record(Run{Kind: KindBackup, Success: true}); err != nil {
		t.Errorf("Record after a corrupt read: %v", err)
	}
}

// A file from a future version is not something this build can interpret, and
// guessing is worse than starting over.
func TestAnUnknownVersionIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"version":99,"last_backup":{"job_id":"x"}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if store.Get().LastBackup != nil {
		t.Error("a state file from another version was read anyway")
	}
}

func TestConcurrentRecording(t *testing.T) {
	store, _ := openStore(t)
	done := make(chan struct{})
	for range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 25 {
				_ = store.Get()
				if err := store.Record(Run{Kind: KindBackup, Success: true}); err != nil {
					t.Errorf("Record: %v", err)
					return
				}
			}
		}()
	}
	for range 4 {
		<-done
	}
}
