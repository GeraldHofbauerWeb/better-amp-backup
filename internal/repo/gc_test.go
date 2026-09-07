package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// commitSnapshot writes a snapshot whose files hold the given contents.
func commitSnapshot(t *testing.T, r *Repository, at time.Time, suffix string,
	contents []string, tags ...string) Manifest {
	t.Helper()

	ctx := context.Background()
	w, err := r.NewSnapshotWriter(NewSnapshotID(at, suffix), "inst", "/AMP")
	if err != nil {
		t.Fatalf("NewSnapshotWriter: %v", err)
	}
	for i, body := range contents {
		res, err := r.Objects().Put(ctx, bytes.NewReader([]byte(body)), "f.dat")
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := w.Add(Entry{
			Path: fmt.Sprintf("file%d.dat", i), Type: TypeFile, Mode: 0o644,
			Size: res.Size, ModTime: at.UnixNano(), Hash: res.Hash,
		}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	m := &Manifest{Root: "/AMP", StartedAt: at, FinishedAt: at, Tags: tags}
	if err := w.Commit(m); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return *m
}

func countObjects(t *testing.T, r *Repository) int {
	t.Helper()
	var n int
	if err := r.Objects().List(context.Background(), func(ObjectInfo) error { n++; return nil }); err != nil {
		t.Fatalf("List: %v", err)
	}
	return n
}

// ancientPrune ignores the grace period, which otherwise protects every object
// a test just wrote.
func ancientPrune(dry bool) PruneOptions {
	return PruneOptions{
		DryRun:      dry,
		GracePeriod: time.Nanosecond,
		EmptyTrash:  true,
		Now:         func() time.Time { return time.Now().Add(time.Hour) },
	}
}

func TestPruneRemovesOnlyUnreferencedObjects(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

	keep := commitSnapshot(t, r, base, "aaaaaa", []string{"shared", "only-in-first"})
	commitSnapshot(t, r, base.Add(time.Hour), "bbbbbb", []string{"shared", "only-in-second"})

	if got := countObjects(t, r); got != 3 {
		t.Fatalf("expected 3 distinct objects, got %d", got)
	}

	// Forget the second snapshot; "only-in-second" becomes garbage, but
	// "shared" is still referenced by the first.
	second, err := r.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if err := r.Forget([]string{second[1].ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	rep, err := r.Prune(context.Background(), ancientPrune(false))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if rep.DeletedObjects != 1 {
		t.Errorf("deleted %d objects, want 1", rep.DeletedObjects)
	}

	// The surviving snapshot must still be fully readable.
	assertSnapshotIntact(t, r, keep.ID)
}

// assertSnapshotIntact checks that every object a snapshot references is still
// present and readable.
func assertSnapshotIntact(t *testing.T, r *Repository, id string) {
	t.Helper()
	ctx := context.Background()
	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		t.Fatalf("OpenIndex(%s): %v", id, err)
	}
	defer closeIdx()
	for {
		e, err := ir.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("index of %s: %v", id, err)
		}
		if e.Type != TypeFile || e.Hash.IsZero() {
			continue
		}
		rc, err := r.Objects().Open(ctx, e.Hash)
		if err != nil {
			t.Fatalf("snapshot %s references %s (%s) which prune deleted: %v",
				id, e.Path, e.Hash, err)
		}
		got, _, err := hash.OfReader(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("reading %s: %v", e.Path, err)
		}
		if got != e.Hash {
			t.Fatalf("object for %s hashes to %s, index says %s", e.Path, got, e.Hash)
		}
	}
}

func TestForgetDoesNotDeleteObjects(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b", "c"})

	before := countObjects(t, r)
	if err := r.Forget([]string{m.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if after := countObjects(t, r); after != before {
		t.Errorf("forget removed %d objects; it must remove none", before-after)
	}
	if snaps, _ := r.ListSnapshots(); len(snaps) != 0 {
		t.Errorf("forgotten snapshot still listed")
	}
}

// Forgetting is meant to be reversible until a prune runs. That is the whole
// reason it is a separate step.
func TestUnforgetRestoresASnapshot(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b"})

	if err := r.Forget([]string{m.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if err := r.Unforget(m.ID); err != nil {
		t.Fatalf("Unforget: %v", err)
	}
	snaps, err := r.ListSnapshots()
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(snaps) != 1 || snaps[0].ID != m.ID {
		t.Fatalf("snapshot not restored: %v", snaps)
	}
	assertSnapshotIntact(t, r, m.ID)
}

// Until the trash is emptied a forgotten snapshot's objects stay put, so an
// accidental forget can be undone.
func TestTrashedSnapshotsKeepTheirObjects(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b"})

	if err := r.Forget([]string{m.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	opts := ancientPrune(false)
	opts.EmptyTrash = false
	rep, err := r.Prune(context.Background(), opts)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if rep.DeletedObjects != 0 {
		t.Errorf("prune deleted %d objects belonging to a trashed snapshot", rep.DeletedObjects)
	}
	if err := r.Unforget(m.ID); err != nil {
		t.Fatalf("Unforget: %v", err)
	}
	assertSnapshotIntact(t, r, m.ID)
}

func TestDryRunDeletesNothing(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b", "c"})
	if err := r.Forget([]string{m.ID}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	before := countObjects(t, r)
	rep, err := r.Prune(context.Background(), ancientPrune(true))
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !rep.DryRun || rep.DeletedObjects != 3 {
		t.Errorf("dry run reported %+v, expected to plan 3 deletions", rep)
	}
	if after := countObjects(t, r); after != before {
		t.Errorf("dry run actually deleted %d objects", before-after)
	}
}

// The grace period is the safety net for a lock bug: an object written moments
// ago may belong to a run whose index is not committed yet.
func TestGracePeriodSparesFreshObjects(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	// An object with no snapshot referencing it at all.
	if _, err := r.Objects().Put(ctx, bytes.NewReader([]byte("in flight")), "x.dat"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rep, err := r.Prune(ctx, PruneOptions{GracePeriod: time.Hour, Now: time.Now})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if rep.DeletedObjects != 0 {
		t.Errorf("deleted %d fresh objects; the grace period should have spared them",
			rep.DeletedObjects)
	}
	if rep.SparedRecent != 1 {
		t.Errorf("SparedRecent = %d, want 1", rep.SparedRecent)
	}
	if countObjects(t, r) != 1 {
		t.Error("the in-flight object is gone")
	}
}

func TestPruneLeavesForeignFilesAlone(t *testing.T) {
	r := newTestRepo(t)
	stray := filepath.Join(r.Root(), dirObjects, "NOTES.txt")
	if err := os.WriteFile(stray, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Prune(context.Background(), ancientPrune(false)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("prune deleted a file it could not identify: %v", err)
	}
}

// The property the plan singled out: after any sequence of snapshots, forgets
// and prunes, every object referenced by a surviving snapshot must still exist.
// A failure here is silent data loss, which is the one bug this project cannot
// afford.
func TestPruneNeverBreaksASurvivingSnapshot(t *testing.T) {
	rng := rand.New(rand.NewSource(20260907))

	for iter := 0; iter < 40; iter++ {
		r := newTestRepo(t)
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		var live []string
		seq := 0

		for step := 0; step < 12; step++ {
			switch rng.Intn(3) {
			case 0, 1: // take a snapshot sharing content with earlier ones
				seq++
				n := rng.Intn(5) + 1
				contents := make([]string, n)
				for i := range contents {
					contents[i] = fmt.Sprintf("blob-%d", rng.Intn(8))
				}
				m := commitSnapshot(t, r,
					base.Add(time.Duration(seq)*time.Hour),
					fmt.Sprintf("%06x", seq), contents)
				live = append(live, m.ID)

			case 2: // forget a random snapshot, then prune
				if len(live) == 0 {
					continue
				}
				victim := rng.Intn(len(live))
				if err := r.Forget([]string{live[victim]}); err != nil {
					t.Fatalf("iter %d: Forget: %v", iter, err)
				}
				live = append(live[:victim], live[victim+1:]...)

				if _, err := r.Prune(context.Background(), ancientPrune(false)); err != nil {
					t.Fatalf("iter %d: Prune: %v", iter, err)
				}
				for _, id := range live {
					assertSnapshotIntact(t, r, id)
				}
			}
		}

		// Final check after the whole sequence.
		for _, id := range live {
			assertSnapshotIntact(t, r, id)
		}
	}
}

func TestCheckPassesOnAHealthyRepository(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b"})
	commitSnapshot(t, r, base.Add(time.Hour), "bbbbbb", []string{"b", "c"})

	for _, readData := range []bool{false, true} {
		rep, err := r.Check(context.Background(), CheckOptions{ReadData: readData})
		if err != nil {
			t.Fatalf("Check(readData=%v): %v", readData, err)
		}
		if len(rep.Problems) != 0 {
			t.Errorf("readData=%v: %v", readData, rep.Problems)
		}
		if rep.Snapshots != 2 || rep.Objects != 3 {
			t.Errorf("readData=%v: %d snapshots, %d objects; want 2 and 3",
				readData, rep.Snapshots, rep.Objects)
		}
		if readData && rep.Rehashed != 3 {
			t.Errorf("re-hashed %d objects, want 3", rep.Rehashed)
		}
	}
}

func TestCheckFindsAMissingObject(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a", "b"})

	// Remove an object behind the index's back, as bit rot or a stray rm would.
	ir, closeIdx, err := r.OpenIndex(m.ID)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	e, err := ir.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	closeIdx()
	if err := r.Objects().Delete(context.Background(), e.Hash); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	rep, err := r.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rep.Problems) == 0 {
		t.Fatal("check passed even though an object referenced by a snapshot is gone")
	}
}

// Silent corruption is the failure mode a backup tool must not have. --read-data
// is the only thing that catches it before a restore does.
func TestCheckReadDataFindsCorruption(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	commitSnapshot(t, r, base, "aaaaaa", []string{"a fairly long body so the file is not trivial"})

	var victim string
	if err := filepath.Walk(filepath.Join(r.Root(), dirObjects),
		func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() && victim == "" {
				victim = p
			}
			return err
		}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0x01
	if err := os.WriteFile(victim, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	// Existence alone still looks fine …
	shallow, err := r.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(shallow.Problems) != 0 {
		t.Logf("shallow check already noticed: %v", shallow.Problems)
	}

	// … but reading the data must not.
	deep, err := r.Check(context.Background(), CheckOptions{ReadData: true})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(deep.Problems) == 0 {
		t.Fatal("--read-data did not notice a flipped bit")
	}
}

func TestCheckDetectsATamperedIndex(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	m := commitSnapshot(t, r, base, "aaaaaa", []string{"a"})

	p := filepath.Join(r.Root(), dirSnapshots, m.ID+indexExt)
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, 0x00)
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	rep, err := r.Check(context.Background(), CheckOptions{})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(rep.Problems) == 0 {
		t.Error("check accepted an index that no longer matches its recorded hash")
	}
}
