package settings

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

func seed(t *testing.T) File {
	t.Helper()
	f := Defaults()
	f.Instance = Instance{Name: "SebsModpackv401", ID: "guid", Root: "/home/amp/.ampdata/instances/SebsModpackv401"}
	return f
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := Open(path, seed(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

// Migrating from the systemd timers must change nothing an operator would
// notice, so the defaults have to be the units' behaviour, not a fresh guess.
func TestDefaultsReproduceTheUnitsTheyReplace(t *testing.T) {
	d := Defaults()
	if every, err := d.Schedule.Interval(); err != nil || every != time.Hour {
		t.Errorf("every = %v (%v), want 1h -- OnCalendar=hourly", every, err)
	}
	if j, err := d.Schedule.JitterWindow(); err != nil || j != 5*time.Minute {
		t.Errorf("jitter = %v (%v), want 5m -- RandomizedDelaySec=300", j, err)
	}
	if d.Schedule.Housekeeping.At != "04:30" {
		t.Errorf("housekeeping at %q, want 04:30", d.Schedule.Housekeeping.At)
	}
	for _, step := range []struct {
		name string
		on   bool
	}{
		{"check", d.Schedule.Housekeeping.Check},
		{"forget", d.Schedule.Housekeeping.Forget},
		{"prune", d.Schedule.Housekeeping.Prune},
		{"empty-trash", d.Schedule.Housekeeping.EmptyTrash},
	} {
		if !step.on {
			t.Errorf("%s is off; the retention unit ran it", step.name)
		}
	}
	if !d.Schedule.Quiesce {
		t.Error("quiesce is off; the backup unit passed --quiesce")
	}
}

func TestPolicyRoundTrip(t *testing.T) {
	for _, p := range []repo.Policy{
		repo.DefaultPolicy(),
		{KeepLast: 1, KeepWithin: 90 * time.Minute, MinSnapshots: 1},
		{KeepYearly: 3, KeepWithin: 0, MinSnapshots: 1, KeepTags: []string{"manual"}},
	} {
		got, err := FromPolicy(p).Policy()
		if err != nil {
			t.Fatalf("Policy(): %v", err)
		}
		if got.KeepWithin != p.KeepWithin || got.KeepLast != p.KeepLast ||
			got.MinSnapshots != p.MinSnapshots || len(got.KeepTags) != len(p.KeepTags) {
			t.Errorf("round trip changed the policy: %+v -> %+v", p, got)
		}
	}
}

func TestValidateRejectsWhatWouldOnlyFailAtThreeInTheMorning(t *testing.T) {
	cases := []struct {
		name  string
		spoil func(*File)
		want  string
	}{
		{"unparseable interval", func(f *File) { f.Schedule.Every = "often" }, "not a duration"},
		{"interval below a minute", func(f *File) { f.Schedule.Every = "10s" }, "never finish"},
		{"jitter wider than the interval", func(f *File) { f.Schedule.Jitter = "2h" }, "shorter than"},
		{"unknown time zone", func(f *File) { f.Schedule.Housekeeping.TZ = "Mars/Olympus" }, "unknown time zone"},
		{"time of day that is not one", func(f *File) { f.Schedule.Housekeeping.At = "25:00" }, "not a time of day"},
		{"housekeeping that does nothing", func(f *File) {
			f.Schedule.Housekeeping.Check = false
			f.Schedule.Housekeeping.Forget = false
			f.Schedule.Housekeeping.Prune = false
			f.Schedule.Housekeeping.EmptyTrash = false
		}, "every step is switched off"},
		{"uncompilable pattern", func(f *File) { f.Exclusions.Patterns = []string{"mods/[unclosed"} }, "unclosed"},
		{"relative instance root", func(f *File) { f.Instance.Root = "relative/path" }, "not absolute"},
		{"policy that keeps nothing", func(f *File) { f.Retention = FromPolicy(repo.Policy{}) }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := seed(t)
			c.spoil(&f)
			err := f.Validate()
			if err == nil {
				t.Fatal("accepted")
			}
			if c.want != "" && !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q should mention %q", err, c.want)
			}
		})
	}
}

// A rejected change must leave both the file and the running daemon exactly as
// they were, or a typo in the editor takes the schedule down with it.
func TestUpdateRejectsWithoutChangingAnything(t *testing.T) {
	store, path := openStore(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Update("gerry", func(f *File) error {
		f.Schedule.Every = "nonsense"
		return nil
	}); err == nil {
		t.Fatal("an invalid interval was accepted")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the file changed despite the update being rejected")
	}
	if store.Get().Schedule.Every != "1h" {
		t.Errorf("in-memory settings changed: %q", store.Get().Schedule.Every)
	}
}

func TestUpdateRecordsWhoChangedIt(t *testing.T) {
	store, _ := openStore(t)
	got, err := store.Update("gerry", func(f *File) error {
		f.Schedule.Every = "30m"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.UpdatedBy != "gerry" {
		t.Errorf("UpdatedBy = %q, want gerry", got.UpdatedBy)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not set")
	}
}

// The instance being backed up comes from the command line. A target path
// arriving over HTTP is a write primitive, not a setting.
func TestUpdateCannotRedirectTheBackup(t *testing.T) {
	store, _ := openStore(t)
	got, err := store.Update("gerry", func(f *File) error {
		f.Instance.Root = "/etc"
		f.Instance.Name = "somewhere-else"
		return nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Instance.Root == "/etc" || got.Instance.Name == "somewhere-else" {
		t.Errorf("the instance was editable through Update: %+v", got.Instance)
	}
}

func TestOpenReloadsWhatWasWritten(t *testing.T) {
	store, path := openStore(t)
	if _, err := store.Update("gerry", func(f *File) error {
		f.Schedule.Every = "15m"
		f.Exclusions.Patterns = []string{"logs", "!logs/keep.log"}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reopened, err := Open(path, seed(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got := reopened.Get()
	if got.Schedule.Every != "15m" {
		t.Errorf("every = %q, want 15m", got.Schedule.Every)
	}
	if len(got.Exclusions.Patterns) != 2 {
		t.Errorf("patterns = %v", got.Exclusions.Patterns)
	}
}

// The command line still decides what is being backed up: a stale root in a
// file nobody remembers editing is a backup of the wrong directory.
func TestOpenTakesTheInstanceFromTheSeed(t *testing.T) {
	store, path := openStore(t)
	_ = store

	moved := seed(t)
	moved.Instance.Root = "/srv/amp/instances/SebsModpackv401"
	reopened, err := Open(path, moved)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := reopened.Get().Instance.Root; got != moved.Instance.Root {
		t.Errorf("root = %q, want the one from the command line", got)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	store, _ := openStore(t)
	got := store.Get()
	got.Exclusions.Patterns = append(got.Exclusions.Patterns, "injected")
	got.Retention.KeepTags = append(got.Retention.KeepTags, "injected")
	if len(store.Get().Exclusions.Patterns) != 0 {
		t.Error("a caller reached into the store's own slice")
	}
	for _, tag := range store.Get().Retention.KeepTags {
		if tag == "injected" {
			t.Error("a caller reached into the store's keep tags")
		}
	}
}

func TestSubscribersSeeAcceptedChangesOnly(t *testing.T) {
	store, _ := openStore(t)
	ch, stop := store.Subscribe()
	defer stop()

	if _, err := store.Update("gerry", func(f *File) error {
		f.Schedule.Every = "nonsense"
		return nil
	}); err == nil {
		t.Fatal("invalid update accepted")
	}
	select {
	case got := <-ch:
		t.Fatalf("a rejected change was published: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}

	if _, err := store.Update("gerry", func(f *File) error {
		f.Schedule.Every = "30m"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	select {
	case got := <-ch:
		if got.Schedule.Every != "30m" {
			t.Errorf("published %q", got.Schedule.Every)
		}
	case <-time.After(time.Second):
		t.Fatal("no notification")
	}
}

// A subscriber that has not drained its last update does not get to stall the
// settings editor.
func TestASleepingSubscriberDoesNotBlockAnUpdate(t *testing.T) {
	store, _ := openStore(t)
	_, stop := store.Subscribe()
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 5 {
			if _, err := store.Update("gerry", func(f *File) error {
				f.Retention.KeepLast = i + 1
				return nil
			}); err != nil {
				t.Errorf("Update: %v", err)
				return
			}
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("updates blocked on a subscriber that never read")
	}
}

func TestConcurrentGetAndUpdate(t *testing.T) {
	store, _ := openStore(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_ = store.Get()
				if _, err := store.Update("gerry", func(f *File) error {
					f.Retention.KeepLast = i + 1
					return nil
				}); err != nil {
					t.Errorf("Update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
