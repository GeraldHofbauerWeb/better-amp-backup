package repo

import (
	"fmt"
	"math/rand"
	"testing"
	"time"
)

var refNow = time.Date(2026, 9, 7, 18, 0, 0, 0, time.UTC)

func snapshotAt(t time.Time, tags ...string) Manifest {
	return Manifest{
		Version:   manifestVersion,
		ID:        NewSnapshotID(t, fmt.Sprintf("%06x", t.Unix()%0xffffff)),
		Instance:  "inst",
		StartedAt: t,
		State:     StateComplete,
		Tags:      tags,
	}
}

// hourlySeries builds n snapshots one hour apart, newest last.
func hourlySeries(n int) []Manifest {
	out := make([]Manifest, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, snapshotAt(refNow.Add(-time.Duration(i)*time.Hour)))
	}
	return out
}

func ids(ms []Manifest) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}

func TestKeepLast(t *testing.T) {
	snaps := hourlySeries(10)
	keep, remove, err := Policy{KeepLast: 3, MinSnapshots: 0}.Split(snaps, refNow)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(keep) != 3 || len(remove) != 7 {
		t.Fatalf("kept %d, removed %d; want 3 and 7", len(keep), len(remove))
	}
	// The kept ones must be the newest, not an arbitrary three.
	newest := snaps[len(snaps)-1].ID
	if keep[0].ID != newest {
		t.Errorf("newest kept = %s, want %s", keep[0].ID, newest)
	}
}

func TestKeepWithin(t *testing.T) {
	snaps := hourlySeries(10)
	keep, _, err := Policy{KeepWithin: 3 * time.Hour, MinSnapshots: 0}.Split(snaps, refNow)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	// Snapshots at now, now-1h and now-2h are strictly newer than now-3h.
	if len(keep) != 3 {
		t.Errorf("kept %d, want 3: %v", len(keep), ids(keep))
	}
}

func TestKeepHourlyBuckets(t *testing.T) {
	// Three snapshots per hour over six hours: hourly retention must keep the
	// newest of each hour and nothing more.
	var snaps []Manifest
	for h := 5; h >= 0; h-- {
		for _, m := range []int{0, 20, 40} {
			snaps = append(snaps, snapshotAt(refNow.Add(-time.Duration(h)*time.Hour).Add(time.Duration(m)*time.Minute)))
		}
	}
	keep, _, err := Policy{KeepHourly: 4, MinSnapshots: 0}.Split(snaps, refNow)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(keep) != 4 {
		t.Fatalf("kept %d, want 4: %v", len(keep), ids(keep))
	}
	seen := map[string]bool{}
	for _, m := range keep {
		h := m.StartedAt.Format("2006-01-02T15")
		if seen[h] {
			t.Errorf("kept two snapshots from hour %s", h)
		}
		seen[h] = true
	}
}

func TestTaggedSnapshotsAlwaysSurvive(t *testing.T) {
	snaps := hourlySeries(20)
	// Tag the oldest, which every other rule would happily discard.
	snaps[0].Tags = []string{"pre-restore"}

	keep, remove, err := Policy{KeepLast: 2, KeepTags: []string{"pre-restore"}}.Split(snaps, refNow)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	for _, m := range remove {
		if m.ID == snaps[0].ID {
			t.Fatal("a pre-restore snapshot was scheduled for removal")
		}
	}
	var found bool
	for _, m := range keep {
		if m.ID == snaps[0].ID {
			found = true
		}
	}
	if !found {
		t.Error("the tagged snapshot is in neither set")
	}
}

func TestMinSnapshotsOverridesEverything(t *testing.T) {
	snaps := hourlySeries(10)
	keep, _, err := Policy{KeepLast: 1, MinSnapshots: 5}.Split(snaps, refNow)
	if err != nil {
		t.Fatalf("Split: %v", err)
	}
	if len(keep) != 5 {
		t.Errorf("kept %d, want 5: the floor must override KeepLast", len(keep))
	}
}

func TestPolicyThatKeepsNothingIsRefused(t *testing.T) {
	if _, _, err := (Policy{}).Split(hourlySeries(5), refNow); err == nil {
		t.Error("an empty policy was accepted; it would delete every snapshot")
	}
	if _, _, err := (Policy{KeepLast: -1}).Split(hourlySeries(5), refNow); err == nil {
		t.Error("a negative keep count was accepted")
	}
}

func TestDecisionsExplainThemselves(t *testing.T) {
	snaps := hourlySeries(5)
	snaps[0].Tags = []string{"manual"}
	decisions, err := DefaultPolicy().Apply(snaps, refNow)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, d := range decisions {
		if d.Keep && len(d.Reasons) == 0 {
			t.Errorf("%s is kept but no rule claims it", d.Manifest.ID)
		}
		if !d.Keep && len(d.Reasons) != 0 {
			t.Errorf("%s is removed but has reasons %v", d.Manifest.ID, d.Reasons)
		}
	}
}

// Property: whatever the input, a policy never keeps fewer than its floor, and
// never removes a tagged snapshot.
func TestPolicyPropertiesHold(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	policy := DefaultPolicy()

	for iter := 0; iter < 300; iter++ {
		n := rng.Intn(60)
		var snaps []Manifest
		for i := 0; i < n; i++ {
			at := refNow.Add(-time.Duration(rng.Intn(24*400)) * time.Hour)
			var tags []string
			if rng.Intn(10) == 0 {
				tags = []string{"manual"}
			}
			snaps = append(snaps, snapshotAt(at, tags...))
		}

		keep, remove, err := policy.Split(snaps, refNow)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if len(keep)+len(remove) != len(snaps) {
			t.Fatalf("iter %d: %d in, %d+%d out", iter, len(snaps), len(keep), len(remove))
		}
		if len(snaps) >= policy.MinSnapshots && len(keep) < policy.MinSnapshots {
			t.Fatalf("iter %d: kept %d of %d, below the floor of %d",
				iter, len(keep), len(snaps), policy.MinSnapshots)
		}
		for _, m := range remove {
			if m.HasTag("manual") {
				t.Fatalf("iter %d: removed a tagged snapshot %s", iter, m.ID)
			}
		}
	}
}

// Property: applying a policy twice changes nothing the second time.
func TestPolicyIsIdempotent(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	policy := DefaultPolicy()

	for iter := 0; iter < 100; iter++ {
		var snaps []Manifest
		for i := 0; i < rng.Intn(40)+1; i++ {
			snaps = append(snaps, snapshotAt(refNow.Add(-time.Duration(rng.Intn(5000))*time.Hour)))
		}

		keep1, _, err := policy.Split(snaps, refNow)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		keep2, remove2, err := policy.Split(keep1, refNow)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if len(remove2) != 0 {
			t.Fatalf("iter %d: re-applying the policy wanted to remove %d more snapshots",
				iter, len(remove2))
		}
		if len(keep2) != len(keep1) {
			t.Fatalf("iter %d: second pass kept %d, first kept %d", iter, len(keep2), len(keep1))
		}
	}
}
