package repo

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The point of the whole tool is that two snapshots describing the same bytes
// store them once. Stats has to show that as a ratio above one, which means
// counting logical bytes per snapshot and stored bytes per object.
func TestStatsCountsSharedContentOncePerObject(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	payload := strings.Repeat("the same gigabyte, twice over\n", 200)
	put, err := r.Objects().Put(ctx, strings.NewReader(payload), "level.dat")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := r.Objects().Put(ctx, strings.NewReader(payload), "level.dat"); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for i, suffix := range []string{"aaaaaa", "bbbbbb"} {
		id := NewSnapshotID(base.Add(time.Duration(i)*time.Hour), suffix)
		w, err := r.NewSnapshotWriter(id, "inst", "/AMP")
		if err != nil {
			t.Fatalf("NewSnapshotWriter: %v", err)
		}
		if err := w.Commit(&Manifest{
			Root: "/AMP", StartedAt: base, FinishedAt: base,
			Stats: Stats{Files: 1, TotalBytes: put.Size},
		}); err != nil {
			t.Fatalf("Commit: %v", err)
		}
	}

	stats, err := r.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Snapshots != 2 {
		t.Errorf("Snapshots = %d, want 2", stats.Snapshots)
	}
	if stats.Objects != 1 {
		t.Errorf("Objects = %d, want 1 -- the second Put was a dedup hit", stats.Objects)
	}
	if want := 2 * put.Size; stats.LogicalBytes != want {
		t.Errorf("LogicalBytes = %d, want %d", stats.LogicalBytes, want)
	}
	if stats.StoredBytes <= 0 || stats.StoredBytes >= stats.LogicalBytes {
		t.Errorf("StoredBytes = %d, want something below LogicalBytes (%d)",
			stats.StoredBytes, stats.LogicalBytes)
	}
	if stats.Ratio() <= 1 {
		t.Errorf("Ratio() = %v, want more than 1", stats.Ratio())
	}
	if stats.SavedBytes() != stats.LogicalBytes-stats.StoredBytes {
		t.Errorf("SavedBytes() = %d, want %d", stats.SavedBytes(), stats.LogicalBytes-stats.StoredBytes)
	}
}

// An empty repository is the first thing a new install reports on, so it has to
// produce a printable zero rather than a division by zero.
func TestStatsOnAnEmptyRepository(t *testing.T) {
	stats, err := newTestRepo(t).Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats != (RepoStats{}) {
		t.Errorf("Stats() = %+v, want the zero value", stats)
	}
	if stats.Ratio() != 0 {
		t.Errorf("Ratio() = %v, want 0", stats.Ratio())
	}
}
