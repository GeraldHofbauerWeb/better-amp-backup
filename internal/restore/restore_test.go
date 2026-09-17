package restore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/backup"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

var hotPatterns = []string{"**/survival_world/**", "**/level.dat*", "**/playerdata/**", "**/*.mca"}

// buildFixture writes a tree that exercises the awkward cases: an empty file,
// an empty directory, a symlink, unusual permissions, names with spaces,
// non-ASCII characters and glob metacharacters, and region-file-shaped
// incompressible blobs.
func buildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	write := func(rel string, body []byte, mode os.FileMode) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, mode); err != nil {
			t.Fatal(err)
		}
	}

	write("Minecraft/server.properties", []byte("level-name=survival_world\n"), 0o644)
	write("Minecraft/run.sh", []byte("#!/bin/sh\nexec java -jar server.jar\n"), 0o755)
	write("Minecraft/eula.txt", []byte("eula=true\n"), 0o600)
	write("Minecraft/empty.dat", nil, 0o644)
	write("Minecraft/mods/Vanilla Plus Additions.jar", randomBytes(32<<10, 1), 0o644)
	write("Minecraft/config/größe-täst.toml", []byte("wert = \"ümlaut\"\n"), 0o644)
	// A name full of glob metacharacters. Modpack jars really look like this,
	// and no pattern the exclude engine can compile selects this file alone.
	write("Minecraft/mods/[1.21.1] Awkward [Name].jar", randomBytes(4<<10, 7), 0o644)
	write("Minecraft/survival_world/level.dat", randomBytes(4<<10, 2), 0o644)
	for i := 0; i < 6; i++ {
		write(fmt.Sprintf("Minecraft/survival_world/region/r.%d.0.mca", i),
			randomBytes(48<<10, int64(10+i)), 0o644)
	}
	write("Minecraft/survival_world/playerdata/uuid.dat", randomBytes(2<<10, 3), 0o644)

	if err := os.MkdirAll(filepath.Join(root, "Minecraft", "empty-dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("2026-09-07-1.log.gz",
		filepath.Join(root, "Minecraft", "latest.log")); err != nil {
		t.Fatal(err)
	}
	return root
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

// snapshotSeq keeps clock and ID unique across every snapshot a test takes,
// so two runs in the same test never collide on an ID.
var snapshotSeq int64

func takeSnapshot(t *testing.T, r *repo.Repository, root string) *repo.Manifest {
	t.Helper()
	seq := atomic.AddInt64(&snapshotSeq, 1)
	now := time.Date(2026, 9, 7, 17, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour)
	m, err := backup.Run(context.Background(), r, backup.Options{
		Instance:    "SebsModpackv401",
		Root:        root,
		Hot:         exclude.MustCompile(hotPatterns),
		ToolVersion: "test",
		Now:         func() time.Time { now = now.Add(time.Second); return now },
		IDSuffix:    func() string { return fmt.Sprintf("%06x", seq) },
	})
	if err != nil {
		t.Fatalf("backup.Run: %v", err)
	}
	return m
}

// TestRoundTrip is the acceptance test for the whole tool: what goes in must
// come out, byte for byte, permissions and links included.
func TestRoundTrip(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := filepath.Join(t.TempDir(), "restored")
	rep, err := Run(context.Background(), r, Options{Snapshot: m.ID, Target: dst})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Files == 0 {
		t.Fatal("restore wrote no files")
	}
	if rep.Verified != rep.Files {
		t.Errorf("verified %d of %d files", rep.Verified, rep.Files)
	}

	compareTrees(t, src, dst)

	// And the tool's own verifier must agree.
	diffs, err := Verify(context.Background(), r, m.ID, dst)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(diffs) != 0 {
		t.Errorf("Verify reported %d differences:\n%v", len(diffs), diffs)
	}
}

// compareTrees walks both sides independently of the snapshot index, so a bug
// that corrupted the index the same way in both directions cannot hide.
func compareTrees(t *testing.T, src, dst string) {
	t.Helper()
	seen := map[string]bool{}

	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		seen[rel] = true
		other := filepath.Join(dst, rel)

		otherInfo, err := os.Lstat(other)
		if err != nil {
			t.Errorf("%s: missing from restore", rel)
			return nil
		}
		if info.Mode().Type() != otherInfo.Mode().Type() {
			t.Errorf("%s: type %v restored as %v", rel, info.Mode().Type(), otherInfo.Mode().Type())
			return nil
		}
		if info.Mode().Perm() != otherInfo.Mode().Perm() {
			t.Errorf("%s: mode %o restored as %o", rel, info.Mode().Perm(), otherInfo.Mode().Perm())
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			a, _ := os.Readlink(p)
			b, _ := os.Readlink(other)
			if a != b {
				t.Errorf("%s: link %q restored as %q", rel, a, b)
			}
		case info.Mode().IsRegular():
			a, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read %s: %v", p, err)
			}
			b, err := os.ReadFile(other)
			if err != nil {
				t.Errorf("%s: %v", rel, err)
				return nil
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s: content differs (%d vs %d bytes)", rel, len(a), len(b))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source: %v", err)
	}

	// Nothing extra may appear in the restore either.
	err = filepath.Walk(dst, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dst, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if !seen[rel] {
			t.Errorf("%s: restore invented a path that was not in the source", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk restore: %v", err)
	}
}

// A snapshot must still describe the state it was taken at, even after the
// live tree has moved on. This is the "world as of 14:00" guarantee.
func TestOldSnapshotRestoresOldContent(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)

	region := filepath.Join(src, "Minecraft", "survival_world", "region", "r.0.0.mca")
	original, err := os.ReadFile(region)
	if err != nil {
		t.Fatal(err)
	}

	first := takeSnapshot(t, r, src)

	// Simulate an hour of play.
	if err := os.WriteFile(region, randomBytes(48<<10, 777), 0o644); err != nil {
		t.Fatal(err)
	}
	second := takeSnapshot(t, r, src)
	if first.ID == second.ID {
		t.Fatal("snapshots share an ID")
	}

	dst := filepath.Join(t.TempDir(), "old")
	if _, err := Run(context.Background(), r, Options{Snapshot: first.ID, Target: dst}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "Minecraft", "survival_world", "region", "r.0.0.mca"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Error("restoring the older snapshot produced the newer content")
	}
}

// Corruption in the repository must stop a restore, not be written out.
func TestCorruptedObjectAbortsRestore(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	// Flip a byte in the payload of some object.
	objectsDir := filepath.Join(r.Root(), "objects")
	var victim string
	err := filepath.Walk(objectsDir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || victim != "" {
			return err
		}
		if info.Size() > 1024 {
			victim = p
		}
		return nil
	})
	if err != nil || victim == "" {
		t.Fatalf("could not find an object to corrupt: %v", err)
	}
	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xFF
	if err := os.WriteFile(victim, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored")
	if _, err := Run(context.Background(), r, Options{Snapshot: m.ID, Target: dst}); err == nil {
		t.Fatal("restore completed despite a corrupted object")
	}
}

func TestRestoreRefusesNonEmptyTarget(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(dst, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(context.Background(), r, Options{Snapshot: m.ID, Target: dst}); err == nil {
		t.Error("restore overwrote a non-empty target without being asked to")
	}
	if _, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: dst, Overwrite: true}); err != nil {
		t.Errorf("restore with Overwrite: %v", err)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := filepath.Join(t.TempDir(), "planned")
	rep, err := Run(context.Background(), r, Options{Snapshot: m.ID, Target: dst, DryRun: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rep.DryRun || rep.Files == 0 {
		t.Errorf("dry run reported %+v", rep)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Error("dry run created the target directory")
	}
}

func TestIncludeRestrictsRestore(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := filepath.Join(t.TempDir(), "world-only")
	_, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: dst,
		Include: exclude.MustCompile([]string{"Minecraft/survival_world"}),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "Minecraft", "survival_world", "level.dat")); err != nil {
		t.Errorf("world was not restored: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "Minecraft", "server.properties")); !os.IsNotExist(err) {
		t.Error("restore wrote files outside the include filter")
	}
}

func TestRestoreRejectsUnknownSnapshot(t *testing.T) {
	r := newRepo(t)
	if _, err := Run(context.Background(), r, Options{
		Snapshot: "20260907T170000Z-aaaaaa", Target: t.TempDir()}); err == nil {
		t.Error("restore accepted a snapshot that does not exist")
	}
	if _, err := Run(context.Background(), r, Options{
		Snapshot: "../../etc", Target: t.TempDir()}); err == nil {
		t.Error("restore accepted a malformed snapshot id")
	}
}

func TestRestoreRequiresTarget(t *testing.T) {
	r := newRepo(t)
	if _, err := Run(context.Background(), r, Options{Snapshot: "x"}); err == nil {
		t.Error("restore accepted an empty target")
	}
}

// A partial snapshot is restorable but imperfect, and the operator has to be
// told. That used to happen with a Fprintf to os.Stderr from inside the
// library, which under the daemon means the journal -- the one place nobody
// watching a restore in a browser will look. It travels in the report now.
func TestPartialSnapshotWarnsThroughTheReport(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)
	markPartial(t, r, m.ID)

	stderr := captureStderr(t)
	rep, err := Run(context.Background(), r, Options{
		Snapshot: m.ID,
		Target:   filepath.Join(t.TempDir(), "restored"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Warnings) != 1 {
		t.Fatalf("Warnings = %v, want exactly one", rep.Warnings)
	}
	if !strings.Contains(rep.Warnings[0], "partial") || !strings.Contains(rep.Warnings[0], m.ID) {
		t.Errorf("warning should name the snapshot and its state, got %q", rep.Warnings[0])
	}
	if got := stderr(); got != "" {
		t.Errorf("the library wrote to stderr: %q", got)
	}
}

// A dry run is where someone looks before committing to the real thing, so the
// warning has to reach them there too.
func TestPartialSnapshotWarnsOnADryRun(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)
	markPartial(t, r, m.ID)

	rep, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: filepath.Join(t.TempDir(), "restored"), DryRun: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rep.Warnings) != 1 {
		t.Errorf("Warnings = %v, want exactly one", rep.Warnings)
	}
}

// markPartial rewrites a committed manifest's state. There is no API for this
// on purpose -- only a backup decides that a snapshot came out partial -- so
// the test edits the file, which is also a check that the state round-trips
// through JSON under the name the manifest actually uses.
func markPartial(t *testing.T, r *repo.Repository, id string) {
	t.Helper()
	path := filepath.Join(r.Root(), "snapshots", id+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding manifest: %v", err)
	}
	doc["state"] = string(repo.StatePartial)
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding manifest: %v", err)
	}
	if err := os.WriteFile(path, patched, 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
	if m, err := r.LoadManifest(id); err != nil || m.State != repo.StatePartial {
		t.Fatalf("manifest did not come back partial: state=%v err=%v", m.State, err)
	}
}

// captureStderr redirects os.Stderr for the duration of the test and returns a
// function yielding whatever was written to it.
func captureStderr(t *testing.T) func() string {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = write

	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		done <- buf.String()
	}()

	var out string
	var once sync.Once
	collect := func() string {
		once.Do(func() {
			os.Stderr = saved
			_ = write.Close()
			out = <-done
			_ = read.Close()
		})
		return out
	}
	t.Cleanup(func() { collect() })
	return collect
}

// The case --include cannot express. The exclude engine has no escape for its
// metacharacters, so there is no pattern that selects this jar and only this
// jar; a path says exactly what it means.
func TestIncludePathTakesNamesLiterally(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := filepath.Join(t.TempDir(), "one-file")
	rep, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: dst,
		IncludePaths: []string{"Minecraft/mods/[1.21.1] Awkward [Name].jar"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Files != 1 {
		t.Errorf("restored %d files, want exactly 1", rep.Files)
	}

	want := filepath.Join(dst, "Minecraft", "mods", "[1.21.1] Awkward [Name].jar")
	got, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("selected file was not restored: %v", err)
	}
	original, err := os.ReadFile(filepath.Join(src, "Minecraft", "mods", "[1.21.1] Awkward [Name].jar"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Error("restored content differs from the source")
	}
	// The sibling in the same directory must be untouched.
	if _, err := os.Stat(filepath.Join(dst, "Minecraft", "mods", "Vanilla Plus Additions.jar")); !os.IsNotExist(err) {
		t.Error("restore reached past the selected path")
	}
}

// Selecting a directory takes everything under it and nothing beside it. This
// is the shape a ticked checkbox produces, and the reason the browser sends a
// minimal cover rather than every path below it.
func TestIncludePathTakesWholeSubtrees(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	dst := filepath.Join(t.TempDir(), "world-only")
	if _, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: dst,
		IncludePaths: []string{"Minecraft/survival_world"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dst, "Minecraft", "survival_world", "playerdata", "uuid.dat")); err != nil {
		t.Errorf("a file deep in the selected subtree is missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "Minecraft", "server.properties")); !os.IsNotExist(err) {
		t.Error("restore wrote a sibling of the selected subtree")
	}
	// The ancestor directory has to exist so the subtree can live in it.
	if info, err := os.Stat(filepath.Join(dst, "Minecraft")); err != nil || !info.IsDir() {
		t.Errorf("ancestor directory missing: %v", err)
	}
}

// Restoring nothing and reporting success is the worst outcome this command
// has: the operator believes their world is back.
func TestIncludePathThatMatchesNothingFails(t *testing.T) {
	src := buildFixture(t)
	r := newRepo(t)
	m := takeSnapshot(t, r, src)

	_, err := Run(context.Background(), r, Options{
		Snapshot: m.ID, Target: filepath.Join(t.TempDir(), "nothing"),
		IncludePaths: []string{"Minecraft/survival_world", "Minecraft/does-not-exist"},
	})
	if err == nil {
		t.Fatal("a selection naming a path the snapshot does not hold must fail")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the missing path, got: %v", err)
	}
}

// Two filters that disagree is a bug report waiting to happen.
func TestIncludeAndIncludePathAreExclusive(t *testing.T) {
	r := newRepo(t)
	_, err := Run(context.Background(), r, Options{
		Snapshot: "20260907T170000Z-aaaaaa", Target: t.TempDir(),
		Include:      exclude.MustCompile([]string{"Minecraft"}),
		IncludePaths: []string{"Minecraft"},
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Errorf("expected a refusal to combine the two filters, got: %v", err)
	}
}
