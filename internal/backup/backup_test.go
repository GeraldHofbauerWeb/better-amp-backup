package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

var hotPatterns = []string{"**/survival_world/**", "**/level.dat*", "**/playerdata/**", "**/*.mca"}

// fixture builds a small stand-in for an AMP instance: a couple of cold files
// that never change and a world whose region files do.
type fixture struct {
	root string
	t    *testing.T
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{root: t.TempDir(), t: t}
	f.write("Minecraft/server.properties", []byte("level-name=survival_world\n"))
	f.write("Minecraft/mods/vanillaplus.jar", randomBytes(64<<10, 1))
	f.write("Minecraft/survival_world/level.dat", []byte("nbt payload"))
	for i := 0; i < 4; i++ {
		f.write(fmt.Sprintf("Minecraft/survival_world/region/r.%d.0.mca", i),
			randomBytes(48<<10, int64(100+i)))
	}
	return f
}

func (f *fixture) write(rel string, body []byte) {
	f.t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func randomBytes(n int, seed int64) []byte {
	out := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(out)
	return out
}

func newRepo(t *testing.T) *repo.Repository {
	t.Helper()
	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	return r
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { t := c.now; c.now = c.now.Add(time.Second); return t }

func baseOptions(root string) Options {
	c := &clock{now: time.Date(2026, 9, 7, 17, 0, 0, 0, time.UTC)}
	var n int64
	return Options{
		Instance:    "SebsModpackv401",
		Root:        root,
		Hot:         exclude.MustCompile(hotPatterns),
		ToolVersion: "test",
		Now:         c.Now,
		IDSuffix:    func() string { return fmt.Sprintf("%06x", atomic.AddInt64(&n, 1)) },
	}
}

func readIndex(t *testing.T, r *repo.Repository, id string) map[string]repo.Entry {
	t.Helper()
	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer closeIdx()
	out := map[string]repo.Entry{}
	for {
		e, err := ir.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out[e.Path] = e
	}
}

func TestFirstRunStoresEverything(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	m, err := Run(context.Background(), r, baseOptions(f.root))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if m.State != repo.StateComplete {
		t.Errorf("state = %q, warnings %v", m.State, m.Warnings)
	}
	if m.Parent != "" {
		t.Errorf("first run has parent %q", m.Parent)
	}
	if m.Stats.Files != 7 {
		t.Errorf("files = %d, want 7", m.Stats.Files)
	}
	if m.Stats.UnchangedFiles != 0 {
		t.Errorf("first run reused %d files; there was nothing to reuse", m.Stats.UnchangedFiles)
	}
	if m.Stats.NewObjects == 0 {
		t.Error("first run stored no objects")
	}

	idx := readIndex(t, r, m.ID)
	for _, p := range []string{
		"Minecraft/server.properties",
		"Minecraft/mods/vanillaplus.jar",
		"Minecraft/survival_world/level.dat",
		"Minecraft/survival_world/region/r.0.0.mca",
	} {
		e, ok := idx[p]
		if !ok {
			t.Errorf("%s missing from index", p)
			continue
		}
		if e.Hash.IsZero() {
			t.Errorf("%s has no content hash", p)
		}
	}
}

// This is the test the whole project exists for: an hour in which nothing
// changed must cost essentially nothing.
func TestUnchangedSecondRunStoresNothing(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)

	first, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}

	second, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.Parent != first.ID {
		t.Errorf("parent = %q, want %q", second.Parent, first.ID)
	}
	if second.Stats.NewObjects != 0 {
		t.Errorf("second run stored %d new objects, want 0", second.Stats.NewObjects)
	}
	if second.Stats.NewBytes != 0 {
		t.Errorf("second run wrote %d new bytes, want 0", second.Stats.NewBytes)
	}
	if second.Stats.UnchangedFiles != first.Stats.Files {
		t.Errorf("stat-diff skipped %d of %d files; every file should have been skipped",
			second.Stats.UnchangedFiles, first.Stats.Files)
	}

	// And the snapshot must still describe the full tree, not just the delta.
	if got, want := len(readIndex(t, r, second.ID)), len(readIndex(t, r, first.ID)); got != want {
		t.Errorf("second index holds %d entries, want %d: an incremental snapshot "+
			"must still be a complete picture", got, want)
	}
}

func TestOnlyChangedFilesAreReRead(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// One region file changes, as if a player wandered into that chunk.
	changed := "Minecraft/survival_world/region/r.1.0.mca"
	f.write(changed, randomBytes(48<<10, 999))

	second, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.Stats.NewObjects != 1 {
		t.Errorf("stored %d new objects, want exactly 1", second.Stats.NewObjects)
	}
	if second.Stats.UnchangedFiles != 6 {
		t.Errorf("skipped %d files, want 6", second.Stats.UnchangedFiles)
	}

	secondIdx := readIndex(t, r, second.ID)
	if secondIdx[changed].Hash.IsZero() {
		t.Error("changed file has no hash in the new snapshot")
	}
}

func TestParanoidModeIgnoresStatDiff(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	opts.Paranoid = true
	second, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}

	if second.Stats.UnchangedFiles != 0 {
		t.Errorf("paranoid run skipped %d files; it must read every one",
			second.Stats.UnchangedFiles)
	}
	// Re-reading must not duplicate anything: the content is identical, so the
	// object store recognises every blob.
	if second.Stats.NewObjects != 0 {
		t.Errorf("paranoid run stored %d new objects; dedup should have caught all",
			second.Stats.NewObjects)
	}
	if second.Stats.ReusedObjects != second.Stats.Files {
		t.Errorf("reused %d of %d files", second.Stats.ReusedObjects, second.Stats.Files)
	}
}

func TestExclusionsKeepPathsOutOfTheSnapshot(t *testing.T) {
	f := newFixture(t)
	f.write("Minecraft/core.104", randomBytes(4<<10, 7))
	f.write("Minecraft/bluemap/web/tiles/x.png", randomBytes(4<<10, 8))
	f.write("Minecraft/bluemap/pluginState.json", []byte("{}"))
	r := newRepo(t)

	opts := baseOptions(f.root)
	opts.Exclude = exclude.MustCompile([]string{"Minecraft/core.*", "Minecraft/bluemap/web"})

	m, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	idx := readIndex(t, r, m.ID)

	for _, p := range []string{"Minecraft/core.104", "Minecraft/bluemap/web/tiles/x.png"} {
		if _, ok := idx[p]; ok {
			t.Errorf("%s should not be in the snapshot", p)
		}
	}
	if _, ok := idx["Minecraft/bluemap/pluginState.json"]; !ok {
		t.Error("bluemap's own config was excluded along with its tiles")
	}
}

// recordingQuiescer proves the hot phase really is bracketed, and that release
// happens no matter what.
type recordingQuiescer struct {
	events     []string
	quiesceErr error
	releaseErr error
	onQuiesce  func()
}

func (q *recordingQuiescer) Name() string { return "recording" }

func (q *recordingQuiescer) Quiesce(context.Context) error {
	q.events = append(q.events, "quiesce")
	if q.onQuiesce != nil {
		q.onQuiesce()
	}
	return q.quiesceErr
}

func (q *recordingQuiescer) Release(context.Context) error {
	q.events = append(q.events, "release")
	return q.releaseErr
}

func TestQuiesceBracketsTheHotPhaseOnly(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	var duringQuiesce []string
	q := &recordingQuiescer{}
	opts := baseOptions(f.root)
	opts.Quiescer = q
	opts.Progress = func(stage string, done, total int) {
		if len(q.events) == 1 && done == 0 {
			duringQuiesce = append(duringQuiesce, stage)
		}
	}

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(q.events) != 2 || q.events[0] != "quiesce" || q.events[1] != "release" {
		t.Fatalf("events = %v, want [quiesce release]", q.events)
	}
	for _, stage := range duringQuiesce {
		if stage == "cold" {
			t.Error("cold files were read while the server was quiesced; that is wasted downtime")
		}
	}
}

func TestReleaseHappensEvenWhenQuiesceFails(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	q := &recordingQuiescer{quiesceErr: errors.New("console timed out")}
	opts := baseOptions(f.root)
	opts.Quiescer = q

	if _, err := Run(context.Background(), r, opts); err == nil {
		t.Fatal("Run succeeded despite a failed quiesce")
	}
	// A half-applied save-off must still be undone, or the server accumulates
	// unsaved world state indefinitely.
	if len(q.events) != 2 || q.events[1] != "release" {
		t.Errorf("events = %v; release must run even after a failed quiesce", q.events)
	}
}

func TestFailedReleaseFailsTheRun(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	q := &recordingQuiescer{releaseErr: errors.New("console unreachable")}
	opts := baseOptions(f.root)
	opts.Quiescer = q

	_, err := Run(context.Background(), r, opts)
	if err == nil {
		t.Fatal("a server left quiesced must not be reported as a successful backup")
	}
	if snaps, _ := r.ListSnapshots(); len(snaps) != 0 {
		t.Errorf("a failed release still committed %d snapshots", len(snaps))
	}
}

func TestVanishedFileDowngradesToPartial(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	doomed := filepath.Join(f.root, "Minecraft", "survival_world", "region", "r.3.0.mca")
	q := &recordingQuiescer{onQuiesce: func() { os.Remove(doomed) }}

	opts := baseOptions(f.root)
	opts.Quiescer = q

	m, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("a file disappearing mid-run should not fail the backup: %v", err)
	}
	if m.State != repo.StatePartial {
		t.Errorf("state = %q, want partial", m.State)
	}
	if len(m.Warnings) == 0 {
		t.Error("no warning recorded for the vanished file")
	}
}

func TestPartialSnapshotIsNotUsedAsParent(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)

	good, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// A file the parent snapshot already covers would be skipped without ever
	// being opened, so its removal would go unnoticed. Use a brand new file,
	// which the run is forced to read, to provoke a genuine partial snapshot.
	f.write("Minecraft/survival_world/region/r.9.0.mca", randomBytes(8<<10, 55))
	doomed := filepath.Join(f.root, "Minecraft", "survival_world", "region", "r.9.0.mca")

	// Copy the options so the shared clock and ID counter keep advancing;
	// only the quiescer differs.
	partialOpts := opts
	partialOpts.Quiescer = &recordingQuiescer{onQuiesce: func() { os.Remove(doomed) }}
	partial, err := Run(context.Background(), r, partialOpts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if partial.State != repo.StatePartial {
		t.Fatalf("expected a partial snapshot, got %q", partial.State)
	}

	third, err := Run(context.Background(), r, opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if third.Parent != good.ID {
		t.Errorf("parent = %q, want the last complete snapshot %q", third.Parent, good.ID)
	}
}

func TestRunRejectsIncompleteOptions(t *testing.T) {
	r := newRepo(t)
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"no instance", Options{Root: "/tmp"}},
		{"no root", Options{Instance: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Run(context.Background(), r, tc.opts); err == nil {
				t.Error("Run accepted incomplete options")
			}
		})
	}
}

func TestSpaceGuardRefusesToFillTheDisk(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)

	opts := baseOptions(f.root)
	// A reserve larger than the whole filesystem can never be satisfied, so the
	// run must be refused before anything is written.
	space, err := r.Space()
	if err != nil {
		t.Fatalf("Space: %v", err)
	}
	opts.ReserveBytes = int64(space.TotalBytes) + (1 << 30)

	_, err = Run(context.Background(), r, opts)
	if err == nil {
		t.Fatal("backup ran despite insufficient free space")
	}
	if !strings.Contains(err.Error(), "refusing to run") {
		t.Errorf("error should say the run was refused, got: %v", err)
	}
	if snaps, _ := r.ListSnapshots(); len(snaps) != 0 {
		t.Errorf("a refused run still committed %d snapshots", len(snaps))
	}
	var objects int
	if err := r.Objects().List(context.Background(), func(repo.ObjectInfo) error {
		objects++
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if objects != 0 {
		t.Errorf("a refused run wrote %d objects; it must write none", objects)
	}
}

func TestSpaceGuardCanBeDisabled(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)
	opts.ReserveBytes = -1 // explicitly off

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("Run with the check disabled: %v", err)
	}
}

func TestSpaceGuardIgnoresUnchangedFiles(t *testing.T) {
	f := newFixture(t)
	r := newRepo(t)
	opts := baseOptions(f.root)
	opts.ReserveBytes = -1

	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Nothing changed, so nothing has to be read, so even a reserve close to
	// the size of the filesystem must not block the run.
	space, err := r.Space()
	if err != nil {
		t.Fatalf("Space: %v", err)
	}
	opts.ReserveBytes = int64(space.AvailableBytes) - (1 << 20)
	if opts.ReserveBytes <= 0 {
		t.Skip("filesystem too full to express this case")
	}
	if _, err := Run(context.Background(), r, opts); err != nil {
		t.Errorf("an incremental run that reads nothing was blocked: %v", err)
	}
}
