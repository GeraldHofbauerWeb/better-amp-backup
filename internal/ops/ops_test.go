package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
)

// recorder is a Progress that remembers, so a test can assert on what the
// operator would have been shown.
type recorder struct {
	stages []string
	logs   []string
}

func (r *recorder) Stage(stage string, done, total int) { r.stages = append(r.stages, stage) }
func (r *recorder) Logf(level, format string, args ...any) {
	r.logs = append(r.logs, level+": "+fmt.Sprintf(format, args...))
}

func (r *recorder) loggedContaining(needle string) bool {
	for _, line := range r.logs {
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}

func fixture(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("server.properties", "level-name=survival_world\n")
	write("survival_world/level.dat", "world data\n")
	write("survival_world/region/r.0.0.mca", "region data\n")
	write("mods/[1.21.1] Awkward.jar", "a mod\n")
	write("logs/latest.log", "noise that the defaults exclude\n")
	return root
}

func newRunner(t *testing.T, root string) (*Runner, *settings.Store, *fake.Server) {
	t.Helper()
	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	cfg := settings.Defaults()
	cfg.Instance = settings.Instance{Name: "Demo", Root: root}
	cfg.Schedule.Quiesce = false
	store, err := settings.NewMemory(cfg)
	if err != nil {
		t.Fatalf("settings.NewMemory: %v", err)
	}

	srv := fake.New("backup", "pw")
	t.Cleanup(srv.Close)
	client, err := amp.New(amp.Config{BaseURL: srv.URL, Username: "backup", Password: "pw"})
	if err != nil {
		t.Fatalf("amp.New: %v", err)
	}

	return &Runner{
		Repo:        r,
		Settings:    store,
		ToolVersion: "test",
		ScratchDir:  t.TempDir(),
		Instance:    func(context.Context) (*amp.Client, error) { return client, nil },
	}, store, srv
}

func TestBackupAppliesTheConfiguredExclusions(t *testing.T) {
	runner, store, _ := newRunner(t, fixture(t))
	var rec recorder

	m, err := runner.Backup(context.Background(), &rec, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if m.State != repo.StateComplete {
		t.Errorf("state = %s", m.State)
	}
	paths := indexPaths(t, runner.Repo, m.ID)
	if paths["logs/latest.log"] {
		t.Error("the default exclusions did not apply: a log file is in the snapshot")
	}
	if !paths["survival_world/level.dat"] {
		t.Error("the world is missing")
	}

	// And a user rule that negates a built-in must win, because it is applied
	// after it. That ordering is the whole reason the assembly lives in one
	// place.
	if _, err := store.Update("test", func(f *settings.File) error {
		f.Exclusions.Patterns = []string{"!logs"}
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	m2, err := runner.Backup(context.Background(), &rec, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if !indexPaths(t, runner.Repo, m2.ID)["logs/latest.log"] {
		t.Error("a user pattern could not negate a built-in one")
	}
}

func TestBackupNeverDescendsIntoItsOwnRepository(t *testing.T) {
	root := fixture(t)
	runner, store, _ := newRunner(t, root)
	// Put the repository inside the instance directory, the way a careless
	// operator would.
	inner, err := repo.Init(filepath.Join(root, "backups"), 3)
	if err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	runner.Repo = inner
	if _, err := store.Update("test", func(f *settings.File) error {
		f.Exclusions.UseDefaults = false
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	m, err := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	for path := range indexPaths(t, inner, m.ID) {
		if strings.HasPrefix(path, "backups/") {
			t.Fatalf("the snapshot contains the repository itself: %s", path)
		}
	}
}

// A restore into the live instance while the server is up would write a world
// underneath a process that has it open.
func TestRestoreIntoTheInstanceRefusesWhileItRuns(t *testing.T) {
	runner, _, srv := newRunner(t, fixture(t))
	m, err := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	srv.SetState(int(amp.StateReady))
	_, err = runner.Restore(context.Background(), &recorder{}, RestoreRequest{
		Snapshot: m.ID, Target: TargetInstance,
	})
	var running *ErrInstanceRunning
	if !errors.As(err, &running) {
		t.Fatalf("err = %v, want ErrInstanceRunning", err)
	}
	if running.State != "ready" {
		t.Errorf("State = %q, want the state the banner should quote", running.State)
	}
}

// Every state other than stopped means a process may still hold those files.
func TestRestoreRefusesEveryStateThatIsNotStopped(t *testing.T) {
	runner, _, srv := newRunner(t, fixture(t))
	m, _ := runner.Backup(context.Background(), &recorder{}, BackupRequest{})

	for _, state := range []amp.State{
		amp.StateReady, amp.StateStarting, amp.StateStopping,
		amp.StateRestarting, amp.StateConfiguring, amp.StatePendingUser,
	} {
		srv.SetState(int(state))
		_, err := runner.Restore(context.Background(), &recorder{}, RestoreRequest{
			Snapshot: m.ID, Target: TargetInstance,
		})
		var running *ErrInstanceRunning
		if !errors.As(err, &running) {
			t.Errorf("state %s was accepted: %v", state, err)
		}
	}
}

func TestRestoreIntoTheInstanceTakesAPreRestoreSnapshotFirst(t *testing.T) {
	root := fixture(t)
	runner, _, srv := newRunner(t, root)
	m, err := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The world changes, then gets restored over.
	worldFile := filepath.Join(root, "survival_world", "level.dat")
	if err := os.WriteFile(worldFile, []byte("ruined\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.SetState(int(amp.StateStopped))
	var rec recorder
	res, err := runner.Restore(context.Background(), &rec, RestoreRequest{
		Snapshot: m.ID, Target: TargetInstance, PreSnapshot: true,
		Paths: []string{"survival_world"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.PreSnapshot == "" {
		t.Fatal("no pre-restore snapshot was taken")
	}

	pre, err := runner.Repo.LoadManifest(res.PreSnapshot)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if !pre.HasTag("pre-restore") {
		t.Errorf("tags = %v, want pre-restore", pre.Tags)
	}
	// The default policy keeps that tag forever, which is what makes it a
	// safety net rather than a snapshot like any other.
	policy := repo.DefaultPolicy()
	var kept bool
	for _, tag := range policy.KeepTags {
		if tag == "pre-restore" {
			kept = true
		}
	}
	if !kept {
		t.Error("the default policy no longer protects pre-restore snapshots")
	}

	back, err := os.ReadFile(worldFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != "world data\n" {
		t.Errorf("the world was not restored: %q", back)
	}
	// And the ruined state is recoverable from the pre-restore snapshot.
	if !rec.loggedContaining(res.PreSnapshot) {
		t.Error("the operator was not told where their old state went")
	}
}

// A dry run must not take a pre-restore snapshot -- it changes nothing, so
// there is nothing to protect.
func TestDryRunRestoreChangesNothing(t *testing.T) {
	root := fixture(t)
	runner, _, srv := newRunner(t, root)
	m, _ := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	srv.SetState(int(amp.StateStopped))

	before, err := runner.Repo.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	res, err := runner.Restore(context.Background(), &recorder{}, RestoreRequest{
		Snapshot: m.ID, Target: TargetInstance, PreSnapshot: true, DryRun: true,
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if res.PreSnapshot != "" {
		t.Error("a dry run took a snapshot")
	}
	if !res.Report.DryRun {
		t.Error("the report does not say it was a dry run")
	}
	after, err := runner.Repo.ListSnapshots()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("snapshot count changed from %d to %d", len(before), len(after))
	}
}

// A scratch restore is the look-before-you-leap one, and it must not need the
// instance to be stopped at all.
func TestScratchRestoreWorksWhileTheServerRuns(t *testing.T) {
	runner, _, srv := newRunner(t, fixture(t))
	m, _ := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	srv.SetState(int(amp.StateReady))

	res, err := runner.Restore(context.Background(), &recorder{}, RestoreRequest{
		Snapshot: m.ID, Target: TargetScratch, Paths: []string{"survival_world/level.dat"},
	})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !strings.HasPrefix(res.TargetPath, runner.ScratchDir) {
		t.Errorf("target %q is outside the scratch directory", res.TargetPath)
	}
	if res.Report.Files != 1 {
		t.Errorf("restored %d files, want 1", res.Report.Files)
	}
	if _, err := os.Stat(filepath.Join(res.TargetPath, "survival_world", "level.dat")); err != nil {
		t.Errorf("the selected file is not there: %v", err)
	}
}

// The daemon must never take a target path from a client.
func TestRestoreRefusesAnUnknownTarget(t *testing.T) {
	runner, _, _ := newRunner(t, fixture(t))
	m, _ := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	_, err := runner.Restore(context.Background(), &recorder{}, RestoreRequest{
		Snapshot: m.ID, Target: TargetKind("/etc"),
	})
	if err == nil || !strings.Contains(err.Error(), "unknown restore target") {
		t.Errorf("err = %v", err)
	}
}

// Deleting from a repository that just failed its own integrity check is how
// one bad snapshot becomes no snapshots.
func TestHousekeepingStopsIfTheCheckFails(t *testing.T) {
	runner, _, _ := newRunner(t, fixture(t))
	m, err := runner.Backup(context.Background(), &recorder{}, BackupRequest{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Remove an object the snapshot references, so the check has something
	// real to find.
	ctx := context.Background()
	var victim hash.Hash
	if err := runner.Repo.Objects().List(ctx, func(info repo.ObjectInfo) error {
		victim = info.Hash
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if victim.IsZero() {
		t.Fatal("the snapshot stored no objects")
	}
	if err := runner.Repo.Objects().Delete(ctx, victim); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var rec recorder
	res, err := runner.Housekeeping(context.Background(), &rec, HousekeepingRequest{
		Check: true, Forget: true, Prune: true,
	})
	if err == nil {
		t.Fatal("housekeeping continued past a failing check")
	}
	if res.Check == nil || len(res.Check.Problems) == 0 {
		t.Errorf("the check reported no problems: %+v", res.Check)
	}
	if res.Prune != nil {
		t.Error("prune ran anyway")
	}
	_ = m
}

func TestExclusionPreviewCountsWhatWouldBeLeftOut(t *testing.T) {
	runner, _, _ := newRunner(t, fixture(t))
	if _, err := runner.Backup(context.Background(), &recorder{}, BackupRequest{}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	preview, err := runner.PreviewExclusions([]string{"survival_world"}, false)
	if err != nil {
		t.Fatalf("PreviewExclusions: %v", err)
	}
	if preview.Excluded == 0 {
		t.Fatal("the preview says nothing would be excluded")
	}
	if preview.Entries <= preview.Excluded {
		t.Errorf("excluded %d of %d entries; the whole snapshot would go", preview.Excluded, preview.Entries)
	}
	var sawWorld bool
	for _, sample := range preview.Samples {
		if strings.HasPrefix(sample, "survival_world/") {
			sawWorld = true
		}
	}
	if !sawWorld {
		t.Errorf("samples = %v, want a path from the world", preview.Samples)
	}

	// An uncompilable rule must be reported, not silently ignored -- that is
	// the difference between an editor you can trust and one you cannot.
	if _, err := runner.PreviewExclusions([]string{"mods/[unclosed"}, false); err == nil {
		t.Error("an invalid pattern was accepted")
	}
}

func indexPaths(t *testing.T, r *repo.Repository, id string) map[string]bool {
	t.Helper()
	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer closeIdx()

	out := map[string]bool{}
	for {
		e, err := ir.Next()
		if err != nil {
			break
		}
		out[e.Path] = true
	}
	return out
}

// The retention preview has to answer for the rules being edited, not the ones
// on disk. Switching a rule off is a decision about which snapshots stop
// existing, and the only honest moment to show that number is before the save.
func TestTheRetentionPreviewMeasuresTheCandidateRules(t *testing.T) {
	runner, store, _ := newRunner(t, fixture(t))
	var rec recorder

	for i := 0; i < 3; i++ {
		if _, err := runner.Backup(context.Background(), &rec, BackupRequest{Paranoid: true}); err != nil {
			t.Fatalf("Backup %d: %v", i, err)
		}
	}

	// The saved policy is the default one, which keeps everything this young.
	saved, err := runner.RetentionPreview(nil)
	if err != nil {
		t.Fatalf("RetentionPreview(nil): %v", err)
	}
	for _, d := range saved {
		if !d.Keep {
			t.Fatalf("the default policy would forget %s, so this test cannot tell the two apart", d.Manifest.ID)
		}
	}

	// Switch every rule off but "keep the last one" -- which is what unticking
	// the rows in the tab produces -- and the answer has to change without
	// anything having been written.
	only := settings.Retention{KeepLast: 1, KeepWithin: "0s"}
	candidate, err := runner.RetentionPreview(&only)
	if err != nil {
		t.Fatalf("RetentionPreview(candidate): %v", err)
	}
	var kept int
	for _, d := range candidate {
		if d.Keep {
			kept++
		}
	}
	if kept != 1 {
		t.Errorf("the candidate rules kept %d snapshot(s), want 1", kept)
	}
	if store.Get().Retention.KeepLast != settings.Defaults().Retention.KeepLast {
		t.Error("previewing a candidate policy changed the saved one")
	}
}

// A rule set that keeps nothing is refused rather than previewed, so the tab
// shows the refusal where the numbers would be instead of a cheerful zero.
func TestAPolicyThatKeepsNothingIsRefused(t *testing.T) {
	runner, _, _ := newRunner(t, fixture(t))
	nothing := settings.Retention{KeepWithin: "0s"}
	if _, err := runner.RetentionPreview(&nothing); err == nil {
		t.Error("a policy with every rule switched off was accepted")
	}
}
