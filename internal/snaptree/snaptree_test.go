package snaptree

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// writeSnapshot builds a snapshot whose index holds exactly these entries.
func writeSnapshot(t *testing.T, r *repo.Repository, id string, entries []repo.Entry) {
	t.Helper()
	w, err := r.NewSnapshotWriter(id, "Demo", "/AMP")
	if err != nil {
		t.Fatalf("NewSnapshotWriter: %v", err)
	}
	for _, e := range entries {
		if err := w.Add(e); err != nil {
			t.Fatalf("Add(%s): %v", e.Path, err)
		}
	}
	base := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	if err := w.Commit(&repo.Manifest{Root: "/AMP", StartedAt: base, FinishedAt: base}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func dirEntry(p string) repo.Entry { return repo.Entry{Path: p, Type: repo.TypeDir, Mode: 0o755} }
func fileEntry(p string, size int64) repo.Entry {
	return repo.Entry{Path: p, Type: repo.TypeFile, Size: size, Mode: 0o644, Hash: hash.Hash{1}}
}

func fixture(t *testing.T) (*repo.Repository, string) {
	t.Helper()
	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatalf("repo.Init: %v", err)
	}
	id := repo.NewSnapshotID(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), "aaaaaa")
	entries := []repo.Entry{
		fileEntry("server.properties", 100),
		dirEntry("mods"),
		fileEntry("mods/[1.21.1] Awkward.jar", 4096),
		fileEntry("mods/Vanilla Plus.jar", 8192),
		dirEntry("survival_world"),
		fileEntry("survival_world/level.dat", 2048),
		dirEntry("survival_world/region"),
		{Path: "survival_world/link", Type: repo.TypeSymlink, Target: "region"},
	}
	for i := range 5 {
		entries = append(entries, fileEntry(fmt.Sprintf("survival_world/region/r.%d.0.mca", i), 1024))
	}
	writeSnapshot(t, r, id, entries)
	return r, id
}

func TestListWalksTheTree(t *testing.T) {
	r, id := fixture(t)
	tree, err := Build(context.Background(), r, id, 0)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	root, total, ok := tree.List("", 0, 0)
	if !ok {
		t.Fatal("the root has no listing")
	}
	if total != 3 {
		t.Fatalf("root holds %d entries: %+v", total, root)
	}
	// Directories first, then by name.
	want := []string{"mods", "survival_world", "server.properties"}
	for i, n := range root {
		if n.Name != want[i] {
			t.Errorf("root[%d] = %q, want %q -- order is directories first, then by name", i, n.Name, want[i])
		}
	}

	// The several spellings of the root all work.
	for _, spelling := range []string{"", "/", ".", "//"} {
		if _, _, ok := tree.List(spelling, 0, 0); !ok {
			t.Errorf("the root is not reachable as %q", spelling)
		}
	}

	if _, _, ok := tree.List("does/not/exist", 0, 0); ok {
		t.Error("an unknown directory returned a listing")
	}
}

// A world's region directory holds thousands of files. Sending all of them to
// a browser at once is how a tab stops responding.
func TestListPages(t *testing.T) {
	r, id := fixture(t)
	tree, _ := Build(context.Background(), r, id, 0)

	first, total, ok := tree.List("survival_world/region", 0, 2)
	if !ok || total != 5 || len(first) != 2 {
		t.Fatalf("page = %d of %d (ok=%v)", len(first), total, ok)
	}
	second, _, _ := tree.List("survival_world/region", 2, 2)
	if len(second) != 2 || second[0].Name == first[0].Name {
		t.Errorf("the second page repeats the first: %v then %v", first, second)
	}
	last, _, _ := tree.List("survival_world/region", 4, 2)
	if len(last) != 1 {
		t.Errorf("the last page holds %d", len(last))
	}
	past, _, ok := tree.List("survival_world/region", 99, 2)
	if !ok || len(past) != 0 {
		t.Errorf("reading past the end returned %v", past)
	}
}

// Ticking a directory has to show what it costs without expanding it.
func TestDirectoriesCarryTheirTotals(t *testing.T) {
	r, id := fixture(t)
	tree, _ := Build(context.Background(), r, id, 0)

	world, ok := tree.Stat("survival_world")
	if !ok {
		t.Fatal("the world directory is missing")
	}
	// level.dat, five region files and the symlink. A symlink is an entry a
	// restore has to recreate, so it counts.
	if world.Children != 7 {
		t.Errorf("Children = %d, want 7", world.Children)
	}
	if want := int64(2048 + 5*1024); world.SubBytes != want {
		t.Errorf("SubBytes = %d, want %d", world.SubBytes, want)
	}

	// The same totals have to appear in the parent's listing, or the browser
	// shows one number when collapsed and another when expanded.
	root, _, _ := tree.List("", 0, 0)
	for _, n := range root {
		if n.Path == "survival_world" && n.Children != world.Children {
			t.Errorf("listing says %d children, Stat says %d", n.Children, world.Children)
		}
	}

	region, _ := tree.Stat("survival_world/region")
	if region.Children != 5 || region.SubBytes != 5*1024 {
		t.Errorf("region = %+v", region)
	}
}

func TestTotalsAndTypes(t *testing.T) {
	r, id := fixture(t)
	tree, _ := Build(context.Background(), r, id, 0)

	if tree.Entries() != 13 {
		t.Errorf("Entries = %d, want 13", tree.Entries())
	}
	if want := int64(100 + 4096 + 8192 + 2048 + 5*1024); tree.Bytes() != want {
		t.Errorf("Bytes = %d, want %d", tree.Bytes(), want)
	}
	link, ok := tree.Stat("survival_world/link")
	if !ok || link.Type != string(repo.TypeSymlink) || link.Target != "region" {
		t.Errorf("symlink = %+v (ok=%v)", link, ok)
	}
}

// A name with glob metacharacters must survive the trip, since it is the very
// case the path-based restore exists for.
func TestAwkwardNamesSurvive(t *testing.T) {
	r, id := fixture(t)
	tree, _ := Build(context.Background(), r, id, 0)

	mods, _, ok := tree.List("mods", 0, 0)
	if !ok {
		t.Fatal("mods has no listing")
	}
	var found bool
	for _, n := range mods {
		if n.Name == "[1.21.1] Awkward.jar" && n.Path == "mods/[1.21.1] Awkward.jar" {
			found = true
		}
	}
	if !found {
		t.Errorf("mods = %+v", mods)
	}
}

func TestSearchFindsByName(t *testing.T) {
	r, id := fixture(t)
	tree, _ := Build(context.Background(), r, id, 0)

	hits := tree.Search("level", 10)
	if len(hits) != 1 || hits[0].Path != "survival_world/level.dat" {
		t.Errorf("Search(level) = %+v", hits)
	}
	if got := tree.Search("MCA", 10); len(got) != 5 {
		t.Errorf("Search is case-sensitive: %d hits", len(got))
	}
	if got := tree.Search("  ", 10); got != nil {
		t.Errorf("an empty query returned %v", got)
	}
	if got := tree.Search("mca", 2); len(got) != 2 {
		t.Errorf("the limit was ignored: %d hits", len(got))
	}
}

// An out-of-memory kill would take the scheduler and the HTTP server with it.
func TestAnOversizedSnapshotIsRefused(t *testing.T) {
	r, id := fixture(t)
	_, err := Build(context.Background(), r, id, 3)
	if err == nil || !strings.Contains(err.Error(), "limit 3") {
		t.Errorf("err = %v, want a refusal naming the limit", err)
	}
}

func TestCacheBuildsOnceForConcurrentCallers(t *testing.T) {
	r, id := fixture(t)
	cache := NewCache(3, time.Minute, 0)

	// Prove it by racing: the tree pointer must be identical for everyone.
	var wg sync.WaitGroup
	trees := make([]*Tree, 16)
	for i := range trees {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree, err := cache.Get(context.Background(), r, id)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			trees[i] = tree
		}()
	}
	wg.Wait()
	for i, tree := range trees {
		if tree == nil || tree != trees[0] {
			t.Fatalf("caller %d got a different tree; it was built more than once", i)
		}
	}
	if cache.Len() != 1 {
		t.Errorf("cache holds %d trees", cache.Len())
	}
}

func TestCacheEvictsIdleTrees(t *testing.T) {
	r, id := fixture(t)
	cache := NewCache(3, time.Minute, 0)
	now := time.Now()
	cache.SetClock(func() time.Time { return now })

	if _, err := cache.Get(context.Background(), r, id); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cache.Len() != 1 {
		t.Fatalf("Len = %d", cache.Len())
	}
	now = now.Add(2 * time.Minute)
	// Any call sweeps; asking for the same tree again rebuilds it.
	if _, err := cache.Get(context.Background(), r, id); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cache.Len() != 1 {
		t.Errorf("Len = %d after an eviction and a rebuild", cache.Len())
	}
}

func TestCacheKeepsOnlyItsBudget(t *testing.T) {
	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	var ids []string
	for i := range 5 {
		id := repo.NewSnapshotID(base.Add(time.Duration(i)*time.Hour), fmt.Sprintf("%06d", i))
		writeSnapshot(t, r, id, []repo.Entry{fileEntry("a.txt", 1)})
		ids = append(ids, id)
	}

	cache := NewCache(2, time.Hour, 0)
	for _, id := range ids {
		if _, err := cache.Get(context.Background(), r, id); err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
	}
	if cache.Len() > 2 {
		t.Errorf("cache holds %d trees, budget is 2", cache.Len())
	}
}

// A build that failed because a request was cancelled must not become a
// permanent error for everyone afterwards.
func TestAFailedBuildIsNotCached(t *testing.T) {
	r, id := fixture(t)
	cache := NewCache(3, time.Minute, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Get(ctx, r, id); err == nil {
		t.Fatal("a cancelled build succeeded")
	}
	if _, err := cache.Get(context.Background(), r, id); err != nil {
		t.Errorf("the next caller inherited the failure: %v", err)
	}
}

func TestInvalidateDropsATree(t *testing.T) {
	r, id := fixture(t)
	cache := NewCache(3, time.Minute, 0)
	first, err := cache.Get(context.Background(), r, id)
	if err != nil {
		t.Fatal(err)
	}
	cache.Invalidate(id)
	second, err := cache.Get(context.Background(), r, id)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("Invalidate did not drop the tree")
	}
}

func TestBuildIsCancellable(t *testing.T) {
	r, err := repo.Init(t.TempDir(), 3)
	if err != nil {
		t.Fatal(err)
	}
	id := repo.NewSnapshotID(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), "bbbbbb")
	var entries []repo.Entry
	for i := range 5000 {
		entries = append(entries, fileEntry(fmt.Sprintf("dir/f%05d.dat", i), 1))
	}
	writeSnapshot(t, r, id, append([]repo.Entry{dirEntry("dir")}, entries...))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, r, id, 0); err == nil {
		t.Error("a cancelled build ran to completion")
	}
}
