package repo

import (
	"fmt"
	"sort"
	"time"
)

// Policy decides which snapshots to keep. The semantics deliberately match
// restic's, because that is the behaviour people already have intuitions about
// and getting clever here would only surprise them.
//
// A snapshot is kept if *any* rule wants it. Rules do not compete.
//
// It deliberately carries no JSON tags. KeepWithin is a time.Duration, which
// marshals as a count of nanoseconds -- unreadable in an API response and
// worse in a settings file a person may edit by hand. Anything that has to
// serialise a policy mirrors it with a type of its own and converts.
type Policy struct {
	// KeepLast keeps the N most recent snapshots regardless of their age.
	KeepLast int

	// The bucket rules keep the newest snapshot within each of the N most
	// recent hours, days, weeks, months or years that contain one.
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int

	// KeepWithin keeps everything younger than this.
	KeepWithin time.Duration

	// KeepTags keeps any snapshot carrying one of these tags. This is the
	// equivalent of AMP's "sticky" flag and of the automatic pre-restore
	// snapshot.
	KeepTags []string

	// MinSnapshots is a floor that overrides every other rule. A policy that
	// would empty the repository is far more likely to be a mistake than an
	// intention.
	MinSnapshots int
}

// DefaultPolicy is a grandfather-father-son schedule suited to hourly backups.
func DefaultPolicy() Policy {
	return Policy{
		KeepLast:     6,
		KeepHourly:   24,
		KeepDaily:    14,
		KeepWeekly:   8,
		KeepMonthly:  12,
		KeepYearly:   2,
		KeepWithin:   48 * time.Hour,
		KeepTags:     []string{"pre-restore", "manual"},
		MinSnapshots: 3,
	}
}

// Validate rejects a policy that would keep nothing at all.
func (p Policy) Validate() error {
	if p.KeepLast < 0 || p.KeepHourly < 0 || p.KeepDaily < 0 ||
		p.KeepWeekly < 0 || p.KeepMonthly < 0 || p.KeepYearly < 0 {
		return fmt.Errorf("retention: keep counts must not be negative")
	}
	if p.KeepLast == 0 && p.KeepHourly == 0 && p.KeepDaily == 0 && p.KeepWeekly == 0 &&
		p.KeepMonthly == 0 && p.KeepYearly == 0 && p.KeepWithin == 0 && len(p.KeepTags) == 0 {
		return fmt.Errorf("retention: this policy keeps nothing; refuse rather than delete everything")
	}
	return nil
}

// Decision explains what a policy concluded about one snapshot.
type Decision struct {
	Manifest Manifest `json:"manifest"`
	Keep     bool     `json:"keep"`
	// Reasons lists every rule that wanted this snapshot kept, so an operator
	// can see why `forget` chose what it chose instead of trusting it blindly.
	Reasons []string `json:"reasons,omitempty"`
}

// Apply partitions snapshots into keep and remove sets.
//
// now is passed in rather than read from the clock so the decision is
// reproducible and testable.
func (p Policy) Apply(snapshots []Manifest, now time.Time) ([]Decision, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}

	// Newest first: every rule below counts backwards from the present.
	ordered := append([]Manifest(nil), snapshots...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].StartedAt.After(ordered[j].StartedAt)
	})

	decisions := make([]Decision, len(ordered))
	for i, m := range ordered {
		decisions[i] = Decision{Manifest: m}
	}

	keep := func(i int, reason string) {
		decisions[i].Keep = true
		decisions[i].Reasons = append(decisions[i].Reasons, reason)
	}

	for i := range ordered {
		if i < p.KeepLast {
			keep(i, fmt.Sprintf("one of the last %d", p.KeepLast))
		}
	}

	if p.KeepWithin > 0 {
		cutoff := now.Add(-p.KeepWithin)
		for i, m := range ordered {
			if m.StartedAt.After(cutoff) {
				keep(i, fmt.Sprintf("within %s", p.KeepWithin))
			}
		}
	}

	for _, tag := range p.KeepTags {
		for i, m := range ordered {
			if m.HasTag(tag) {
				keep(i, "tagged "+tag)
			}
		}
	}

	buckets := []struct {
		count int
		name  string
		key   func(time.Time) string
	}{
		{p.KeepHourly, "hourly", func(t time.Time) string { return t.UTC().Format("2006-01-02T15") }},
		{p.KeepDaily, "daily", func(t time.Time) string { return t.UTC().Format("2006-01-02") }},
		{p.KeepWeekly, "weekly", func(t time.Time) string {
			y, w := t.UTC().ISOWeek()
			return fmt.Sprintf("%04d-W%02d", y, w)
		}},
		{p.KeepMonthly, "monthly", func(t time.Time) string { return t.UTC().Format("2006-01") }},
		{p.KeepYearly, "yearly", func(t time.Time) string { return t.UTC().Format("2006") }},
	}
	for _, b := range buckets {
		if b.count <= 0 {
			continue
		}
		seen := map[string]bool{}
		for i, m := range ordered {
			k := b.key(m.StartedAt)
			if seen[k] {
				continue
			}
			if len(seen) >= b.count {
				break
			}
			seen[k] = true
			keep(i, fmt.Sprintf("newest of its %s bucket", b.name))
		}
	}

	// The floor is applied last and walks from newest to oldest, so that if it
	// has to rescue snapshots it rescues the most useful ones.
	if p.MinSnapshots > 0 {
		kept := 0
		for _, d := range decisions {
			if d.Keep {
				kept++
			}
		}
		for i := range decisions {
			if kept >= p.MinSnapshots {
				break
			}
			if !decisions[i].Keep {
				keep(i, fmt.Sprintf("floor of %d snapshots", p.MinSnapshots))
				kept++
			}
		}
	}

	return decisions, nil
}

// Split is a convenience over Apply.
func (p Policy) Split(snapshots []Manifest, now time.Time) (keep, remove []Manifest, err error) {
	decisions, err := p.Apply(snapshots, now)
	if err != nil {
		return nil, nil, err
	}
	for _, d := range decisions {
		if d.Keep {
			keep = append(keep, d.Manifest)
		} else {
			remove = append(remove, d.Manifest)
		}
	}
	return keep, remove, nil
}
