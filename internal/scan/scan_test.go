package scan

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// instanceFixture builds a miniature version of the production layout, so the
// tier and exclusion rules are exercised against the paths they were written
// for rather than against invented ones.
func instanceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"Minecraft/server.properties":                 "level-name=survival_world",
		"Minecraft/run.sh":                            "#!/bin/sh\n",
		"Minecraft/mods/vanillaplus.jar":              "jar bytes",
		"Minecraft/config/bluemap/core.conf":          "accept-download: true",
		"Minecraft/survival_world/level.dat":          "nbt",
		"Minecraft/survival_world/region/r.0.0.mca":   "region bytes",
		"Minecraft/survival_world/region/r.-1.0.mca":  "more region bytes",
		"Minecraft/survival_world/playerdata/a.dat":   "player",
		"Minecraft/survival_world_old/region/r.0.mca": "stale",
		"Minecraft/bluemap/web/tiles/x.png":           "tile",
		"Minecraft/bluemap/pluginState.json":          "{}",
		"Minecraft/core.104":                          "core dump",
		"Minecraft/logs/latest.log":                   "log line",
		"Backups/20260907-170000-abc.zip":             "amp backup",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("2026-09-07-1.log", filepath.Join(root, "Minecraft/logs/link.log")); err != nil {
		t.Fatal(err)
	}
	return root
}

var hotPatterns = []string{
	"**/survival_world/**", "**/level.dat*", "**/playerdata/**", "**/*.mca",
}

func pathsOf(items []Item) map[string]Item {
	out := make(map[string]Item, len(items))
	for _, it := range items {
		out[it.Path] = it
	}
	return out
}

func TestWalkClassifiesTiers(t *testing.T) {
	root := instanceFixture(t)
	res, err := Walk(context.Background(), Options{
		Root: root,
		Hot:  exclude.MustCompile(hotPatterns),
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := pathsOf(res.Items)

	hot := []string{
		"Minecraft/survival_world/level.dat",
		"Minecraft/survival_world/region/r.0.0.mca",
		"Minecraft/survival_world/playerdata/a.dat",
	}
	for _, p := range hot {
		it, ok := got[p]
		if !ok {
			t.Fatalf("%s missing from walk", p)
		}
		if it.Tier != Hot {
			t.Errorf("%s is tier %s, want hot: it must be read under quiesce", p, it.Tier)
		}
	}

	cold := []string{
		"Minecraft/mods/vanillaplus.jar",
		"Minecraft/server.properties",
		"Minecraft/config/bluemap/core.conf",
	}
	for _, p := range cold {
		it, ok := got[p]
		if !ok {
			t.Fatalf("%s missing from walk", p)
		}
		if it.Tier != Cold {
			t.Errorf("%s is tier %s, want cold: quiescing for it would be wasted time", p, it.Tier)
		}
	}
}

func TestWalkAppliesExclusions(t *testing.T) {
	root := instanceFixture(t)
	// Exactly the rules written to the production instance.
	set := exclude.MustCompile([]string{
		"Backups",
		"Minecraft/survival_world_old",
		"Minecraft/core.*",
		"Minecraft/bluemap/web",
		"Minecraft/logs",
	})

	res, err := Walk(context.Background(), Options{Root: root, Exclude: set})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := pathsOf(res.Items)

	for _, p := range []string{
		"Minecraft/survival_world_old/region/r.0.mca",
		"Minecraft/core.104",
		"Minecraft/bluemap/web/tiles/x.png",
		"Minecraft/logs/latest.log",
		"Backups/20260907-170000-abc.zip",
	} {
		if _, ok := got[p]; ok {
			t.Errorf("%s should have been excluded", p)
		}
	}
	for _, p := range []string{
		"Minecraft/survival_world/region/r.0.0.mca",
		"Minecraft/bluemap/pluginState.json",
		"Minecraft/mods/vanillaplus.jar",
	} {
		if _, ok := got[p]; !ok {
			t.Errorf("%s must survive the exclusions", p)
		}
	}
	if res.Excluded == 0 {
		t.Error("Excluded counter stayed at zero")
	}
}

func TestWalkRecordsSymlinksWithoutFollowing(t *testing.T) {
	root := instanceFixture(t)
	res, err := Walk(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	it, ok := pathsOf(res.Items)["Minecraft/logs/link.log"]
	if !ok {
		t.Fatal("symlink missing from walk")
	}
	if it.Type != repo.TypeSymlink {
		t.Errorf("type = %q, want symlink", it.Type)
	}
	if it.Target != "2026-09-07-1.log" {
		t.Errorf("target = %q", it.Target)
	}
	// The link is dangling; a walk that followed it would have failed by now.
}

func TestWalkSkipsRepositoryInsideRoot(t *testing.T) {
	root := instanceFixture(t)
	repoDir := filepath.Join(root, "amp-bb-repo")
	if err := os.MkdirAll(filepath.Join(repoDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "objects", "blob"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Walk(context.Background(), Options{Root: root, SkipAbs: []string{repoDir}})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	for _, it := range res.Items {
		if it.Path == "amp-bb-repo" || filepath.ToSlash(it.Path) == "amp-bb-repo/objects/blob" {
			t.Errorf("walk descended into its own repository: %s", it.Path)
		}
	}
}

func TestWalkIsDeterministic(t *testing.T) {
	root := instanceFixture(t)
	first, err := Walk(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	second, err := Walk(context.Background(), Options{Root: root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(first.Items) != len(second.Items) {
		t.Fatalf("item counts differ: %d vs %d", len(first.Items), len(second.Items))
	}
	for i := range first.Items {
		if first.Items[i].Path != second.Items[i].Path {
			t.Fatalf("order differs at %d: %q vs %q",
				i, first.Items[i].Path, second.Items[i].Path)
		}
	}
}

func TestWalkHonoursContextCancellation(t *testing.T) {
	root := instanceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Walk(ctx, Options{Root: root}); err == nil {
		t.Error("Walk ignored a cancelled context")
	}
}

func TestSplitPartitionsByTier(t *testing.T) {
	items := []Item{
		{Path: "a", Tier: Cold}, {Path: "b", Tier: Hot},
		{Path: "c", Tier: Cold}, {Path: "d", Tier: Hot},
	}
	cold, hot := Split(items)
	if len(cold) != 2 || len(hot) != 2 {
		t.Fatalf("split gave %d cold / %d hot, want 2/2", len(cold), len(hot))
	}
	if cold[0].Path != "a" || cold[1].Path != "c" {
		t.Errorf("cold order lost: %v", cold)
	}
	if hot[0].Path != "b" || hot[1].Path != "d" {
		t.Errorf("hot order lost: %v", hot)
	}
}

func TestUnchanged(t *testing.T) {
	base := Item{
		Path: "r.0.0.mca", Type: repo.TypeFile, Mode: 0o644,
		Size: 614400, ModTime: 1757260800123456789, Inode: 42,
	}
	prev := repo.Entry{
		Path: "r.0.0.mca", Type: repo.TypeFile, Mode: 0o644,
		Size: 614400, ModTime: 1757260800123456789, Inode: 42,
		Hash: hash.Sum([]byte("region")),
	}

	if !Unchanged(prev, base) {
		t.Fatal("identical stat data should count as unchanged")
	}

	// Every one of these must force a re-read. A false "unchanged" here would
	// silently drop a player's progress from the backup.
	mutations := map[string]func(*repo.Entry, *Item){
		"size changed":    func(_ *repo.Entry, i *Item) { i.Size++ },
		"mtime changed":   func(_ *repo.Entry, i *Item) { i.ModTime++ },
		"mode changed":    func(_ *repo.Entry, i *Item) { i.Mode = 0o600 },
		"inode changed":   func(_ *repo.Entry, i *Item) { i.Inode = 99 },
		"type changed":    func(_ *repo.Entry, i *Item) { i.Type = repo.TypeSymlink },
		"prev has nohash": func(p *repo.Entry, _ *Item) { p.Hash = hash.Zero },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p, i := prev, base
			mutate(&p, &i)
			if Unchanged(p, i) {
				t.Errorf("%s was reported as unchanged", name)
			}
		})
	}
}

func TestUnchangedIgnoresMissingInode(t *testing.T) {
	// An index written before inodes were recorded must not force a full
	// re-read of the entire instance.
	prev := repo.Entry{Type: repo.TypeFile, Mode: 0o644, Size: 10, ModTime: 5,
		Hash: hash.Sum([]byte("x"))}
	it := Item{Type: repo.TypeFile, Mode: 0o644, Size: 10, ModTime: 5, Inode: 77}
	if !Unchanged(prev, it) {
		t.Error("a missing inode on either side should not count as a change")
	}
}

func TestWalkRejectsEmptyRoot(t *testing.T) {
	if _, err := Walk(context.Background(), Options{}); err == nil {
		t.Error("Walk accepted an empty root")
	}
}
