package snaptree

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// The production instance holds about 3 700 entries per snapshot and `amp-bb
// ls` streams one in a tenth of a second. This measures the same work plus the
// tree on top, at ten times that size, so there is a number to compare against
// if the build ever gets slower.
func BenchmarkBuild(b *testing.B) {
	r, err := repo.Init(b.TempDir(), 3)
	if err != nil {
		b.Fatal(err)
	}
	id := repo.NewSnapshotID(time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC), "bee111")

	w, err := r.NewSnapshotWriter(id, "Demo", "/AMP")
	if err != nil {
		b.Fatal(err)
	}
	for d := range 40 {
		dir := fmt.Sprintf("world/dim%02d", d)
		if err := w.Add(repo.Entry{Path: dir, Type: repo.TypeDir, Mode: 0o755}); err != nil {
			b.Fatal(err)
		}
		for f := range 1000 {
			e := repo.Entry{
				Path: fmt.Sprintf("%s/r.%d.%d.mca", dir, f/32, f%32),
				Type: repo.TypeFile, Size: int64(f + 1), Mode: 0o644,
				Hash: hash.Hash{1, byte(d), byte(f), byte(f >> 8)},
			}
			if err := w.Add(e); err != nil {
				b.Fatal(err)
			}
		}
	}
	base := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	if err := w.Commit(&repo.Manifest{Root: "/AMP", StartedAt: base, FinishedAt: base}); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		tree, err := Build(context.Background(), r, id, 0)
		if err != nil {
			b.Fatal(err)
		}
		if tree.Entries() != 40040 {
			b.Fatalf("entries = %d", tree.Entries())
		}
	}
}
