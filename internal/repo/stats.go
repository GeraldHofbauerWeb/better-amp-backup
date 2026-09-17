package repo

import "context"

// RepoStats is the accounting behind `amp-bb stats` and the web interface's
// status page.
//
// Producing it walks every object in the store, so it is not free: a caller
// that shows it on each page load has to cache it rather than recompute it.
type RepoStats struct {
	Snapshots int   `json:"snapshots"`
	Objects   int64 `json:"objects"`
	// StoredBytes is what the repository occupies, after deduplication and
	// compression. LogicalBytes is what its snapshots describe between them,
	// counting shared content once per snapshot that references it.
	StoredBytes  int64 `json:"stored_bytes"`
	LogicalBytes int64 `json:"logical_bytes"`
}

// Stats walks the object store and the snapshot manifests.
func (r *Repository) Stats(ctx context.Context) (RepoStats, error) {
	var s RepoStats
	if err := r.Objects().List(ctx, func(info ObjectInfo) error {
		s.Objects++
		s.StoredBytes += info.StoredSize
		return nil
	}); err != nil {
		return RepoStats{}, err
	}

	all, err := r.ListSnapshots()
	if err != nil {
		return RepoStats{}, err
	}
	s.Snapshots = len(all)
	for _, m := range all {
		s.LogicalBytes += m.Stats.TotalBytes
	}
	return s, nil
}

// Ratio is how many bytes of described content each stored byte carries. It is
// zero for an empty repository rather than an infinity or a NaN, because this
// number is printed, and "0.0x" is at least readable.
func (s RepoStats) Ratio() float64 {
	if s.StoredBytes == 0 {
		return 0
	}
	return float64(s.LogicalBytes) / float64(s.StoredBytes)
}

// SavedBytes is what storing the snapshots naively would have cost on top of
// what they actually occupy. It can be negative for a repository holding only
// incompressible data, which is worth seeing rather than clamping away.
func (s RepoStats) SavedBytes() int64 { return s.LogicalBytes - s.StoredBytes }
