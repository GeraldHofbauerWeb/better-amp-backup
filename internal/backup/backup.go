// Package backup orchestrates a single snapshot run: walk, diff, read the
// changed files, and commit.
//
// The run is split into a cold phase and a hot phase. Cold paths — mods,
// configs, jars — are read while the server runs normally, because nothing is
// writing them. Only the hot paths, the ones Minecraft rewrites in place, are
// read between save-off and save-on, and of those only the ones the stat-diff
// says actually changed. On an idle hour that is nothing at all, which is how
// the quiesce window stays in the hundreds of milliseconds.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/scan"
)

// maxRereads bounds how often a file that changed mid-read is retried before
// the snapshot is downgraded to partial.
const maxRereads = 3

// Options configures one run.
type Options struct {
	// Instance is the AMP instance name; it groups snapshots and is what
	// LatestFor keys on.
	Instance   string
	InstanceID string
	// Root is the absolute instance directory.
	Root string

	Exclude *exclude.Set
	Hot     *exclude.Set

	// Quiescer holds the application still for the hot phase.
	Quiescer Quiescer

	// Tags are attached to the manifest; retention can be told to keep them.
	Tags []string

	// Paranoid re-reads and re-hashes every file instead of trusting the
	// stat-diff. Slow, but the escape hatch if an application turns out not to
	// update timestamps reliably.
	Paranoid bool

	// SkipAbs keeps the walk out of directories such as the repository itself.
	SkipAbs []string

	// ReserveBytes is how much free space must remain on the repository's
	// filesystem after the run. A backup that would eat into it is refused
	// before a single object is written.
	//
	// This is not paranoia about a rounding error: the repository usually sits
	// on the same disk as the very server being backed up, and on a shared box
	// it sits alongside unrelated services. Filling that disk is a worse
	// outcome than skipping a snapshot, so the check is on by default and has
	// to be switched off deliberately.
	ReserveBytes int64

	ToolVersion string

	// Now and IDSuffix exist so tests can produce deterministic snapshot IDs.
	Now      func() time.Time
	IDSuffix func() string
	Progress func(stage string, done, total int)
}

func (o *Options) applyDefaults() {
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.IDSuffix == nil {
		o.IDSuffix = randomSuffix
	}
	if o.Quiescer == nil {
		o.Quiescer = NoQuiesce{Reason: "not configured"}
	}
	if o.Progress == nil {
		o.Progress = func(string, int, int) {}
	}
	if o.ReserveBytes == 0 {
		o.ReserveBytes = DefaultReserveBytes
	}
	if o.ReserveBytes < 0 {
		o.ReserveBytes = 0 // caller explicitly disabled the check
	}
}

// DefaultReserveBytes is the free space a run refuses to consume. Two
// gigabytes is enough headroom for a database on the same disk to keep
// writing while an operator notices and intervenes.
const DefaultReserveBytes = 2 << 30

func (o Options) validate() error {
	if o.Instance == "" {
		return errors.New("backup: no instance name")
	}
	if o.Root == "" {
		return errors.New("backup: no instance root")
	}
	return nil
}

// Run takes one snapshot and returns its committed manifest.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*repo.Manifest, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	opts.applyDefaults()

	unlock, err := r.LockWrite(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	started := opts.Now()

	scanned, err := scan.Walk(ctx, scan.Options{
		Root: opts.Root, Exclude: opts.Exclude, Hot: opts.Hot, SkipAbs: opts.SkipAbs,
	})
	if err != nil {
		return nil, fmt.Errorf("backup: scan: %w", err)
	}

	parentID, err := r.LatestFor(opts.Instance)
	if err != nil {
		return nil, err
	}
	previous, err := loadParentIndex(r, parentID)
	if err != nil {
		return nil, err
	}

	if err := checkSpace(r, scanned.Items, previous, opts); err != nil {
		return nil, err
	}

	run := &runner{
		repo:     r,
		opts:     opts,
		previous: previous,
		entries:  make(map[string]repo.Entry, len(scanned.Items)),
		warnings: append([]string(nil), scanned.Warnings...),
	}

	cold, hot := scan.Split(scanned.Items)

	// Cold phase: the server keeps running throughout.
	if err := run.process(ctx, "cold", cold); err != nil {
		return nil, err
	}

	// Hot phase: everything between Quiesce and Release is on the clock.
	quiesceStart := opts.Now()
	if err := opts.Quiescer.Quiesce(ctx); err != nil {
		// Release anyway: Quiesce may have half-applied before failing.
		releaseQuietly(opts.Quiescer)
		return nil, fmt.Errorf("backup: quiesce via %s: %w", opts.Quiescer.Name(), err)
	}
	hotErr := run.process(ctx, "hot", hot)
	releaseErr := opts.Quiescer.Release(ctx)
	quiesceMillis := opts.Now().Sub(quiesceStart).Milliseconds()

	if hotErr != nil {
		return nil, hotErr
	}
	if releaseErr != nil {
		// The snapshot data is fine, but the server may still be held. That is
		// serious enough to fail the run loudly rather than report success.
		return nil, fmt.Errorf("backup: release %s: %w", opts.Quiescer.Name(), releaseErr)
	}

	// Commit in the walk's sorted order so snapshots are reproducible.
	writer, err := r.NewSnapshotWriter(
		repo.NewSnapshotID(started, opts.IDSuffix()), opts.Instance, opts.Root)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(run.entries))
	for p := range run.entries {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := writer.Add(run.entries[p]); err != nil {
			writer.Abort()
			return nil, err
		}
	}

	// Only a warning degrades a snapshot. A re-read is not a warning: it is
	// the retry working. Files change under a running server constantly, so
	// counting successful re-reads here marked every live snapshot partial --
	// and since a stat-diff only descends from a complete parent, the baseline
	// would then freeze at the last snapshot taken while the server was down
	// and never advance again. The tool would quietly get slower for as long
	// as the server stayed up. A file that never settles still warns, above.
	state := repo.StateComplete
	if len(run.warnings) > 0 {
		state = repo.StatePartial
	}

	m := &repo.Manifest{
		InstanceID:    opts.InstanceID,
		Root:          opts.Root,
		StartedAt:     started,
		FinishedAt:    opts.Now(),
		QuiesceMillis: quiesceMillis,
		Parent:        parentID,
		State:         state,
		Tags:          opts.Tags,
		Stats:         run.stats,
		ToolVersion:   opts.ToolVersion,
		Warnings:      run.warnings,
	}
	if err := writer.Commit(m); err != nil {
		writer.Abort()
		return nil, err
	}
	return m, nil
}

type runner struct {
	repo     *repo.Repository
	opts     Options
	previous map[string]repo.Entry
	entries  map[string]repo.Entry
	stats    repo.Stats
	warnings []string
}

func (r *runner) process(ctx context.Context, stage string, items []scan.Item) error {
	for i, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.opts.Progress(stage, i, len(items))

		switch it.Type {
		case repo.TypeDir:
			r.stats.Dirs++
			r.entries[it.Path] = it.Entry()
			continue
		case repo.TypeSymlink:
			r.stats.Symlinks++
			r.entries[it.Path] = it.Entry()
			continue
		}

		entry, err := r.storeFile(ctx, it)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Deleted between the walk and the read. Normal on a live
				// server; the file simply is not in this snapshot.
				r.warnings = append(r.warnings,
					fmt.Sprintf("%s: vanished before it could be read", it.Path))
				continue
			}
			if errors.Is(err, os.ErrPermission) {
				r.warnings = append(r.warnings, fmt.Sprintf("%s: %v", it.Path, err))
				continue
			}
			return fmt.Errorf("backup: %s: %w", it.Path, err)
		}
		r.stats.Files++
		r.stats.TotalBytes += entry.Size
		r.entries[it.Path] = entry
	}
	r.opts.Progress(stage, len(items), len(items))
	return nil
}

// storeFile returns the index entry for one regular file, reading it only when
// the previous snapshot cannot vouch for its contents.
func (r *runner) storeFile(ctx context.Context, it scan.Item) (repo.Entry, error) {
	// A file the previous snapshot already vouches for is never opened. One
	// consequence worth knowing: if such a file is deleted between the walk and
	// this point, the deletion goes unnoticed and the snapshot still lists it.
	// That is deliberate — the entry describes the state at walk time and its
	// content is already in the repository, so the snapshot stays restorable —
	// and it is why deletions are only ever observed by the next walk.
	if !r.opts.Paranoid {
		if prev, ok := r.previous[it.Path]; ok && scan.Unchanged(prev, it) {
			r.stats.UnchangedFiles++
			entry := it.Entry()
			entry.Hash = prev.Hash
			return entry, nil
		}
	}

	for attempt := 0; ; attempt++ {
		f, err := os.Open(it.Abs)
		if err != nil {
			return repo.Entry{}, err
		}
		res, err := r.repo.Objects().Put(ctx, f, it.Path)
		closeErr := f.Close()
		if err != nil {
			return repo.Entry{}, err
		}
		if closeErr != nil {
			return repo.Entry{}, closeErr
		}

		// Re-stat: if the file moved under us the bytes we just stored are a
		// mix of two states and must not be recorded as either.
		after, statErr := os.Lstat(it.Abs)
		if statErr != nil {
			return repo.Entry{}, statErr
		}
		stable := after.Size() == it.Size && after.ModTime().UnixNano() == it.ModTime

		if stable || attempt >= maxRereads-1 {
			if !stable {
				r.stats.RereadFiles++
				r.warnings = append(r.warnings, fmt.Sprintf(
					"%s: still changing after %d reads; snapshot marked partial",
					it.Path, maxRereads))
			}
			if res.New {
				r.stats.NewObjects++
				r.stats.NewBytes += res.StoredSize
			} else {
				r.stats.ReusedObjects++
			}
			entry := it.Entry()
			entry.Hash = res.Hash
			entry.Size = res.Size
			return entry, nil
		}

		// Retry against the file's new state.
		it.Size = after.Size()
		it.ModTime = after.ModTime().UnixNano()
		r.stats.RereadFiles++
	}
}

func loadParentIndex(r *repo.Repository, id string) (map[string]repo.Entry, error) {
	if id == "" {
		return map[string]repo.Entry{}, nil
	}
	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		return nil, fmt.Errorf("backup: open parent index %s: %w", id, err)
	}
	defer closeIdx()

	out := make(map[string]repo.Entry, 4096)
	for {
		e, err := ir.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("backup: read parent index %s: %w", id, err)
		}
		out[e.Path] = e
	}
}

func releaseQuietly(q Quiescer) {
	// A bounded, independent context: the parent may already be cancelled, and
	// releasing matters more than honouring that cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = q.Release(ctx)
}

// checkSpace refuses a run that would leave the repository's filesystem with
// less than ReserveBytes free.
//
// The estimate is the plaintext size of everything that has to be read, which
// deliberately over-estimates: compression and deduplication both reduce what
// is actually written, so a run that passes this check has real headroom.
func checkSpace(r *repo.Repository, items []scan.Item, previous map[string]repo.Entry, opts Options) error {
	if opts.ReserveBytes <= 0 {
		return nil
	}

	var needed int64
	for _, it := range items {
		if it.Type != repo.TypeFile {
			continue
		}
		if !opts.Paranoid {
			if prev, ok := previous[it.Path]; ok && scan.Unchanged(prev, it) {
				continue
			}
		}
		needed += it.Size
	}

	space, err := r.Space()
	if err != nil {
		return err
	}
	if needed+opts.ReserveBytes > int64(space.AvailableBytes) {
		return fmt.Errorf("backup: refusing to run: this snapshot would read up to %s "+
			"but only %s is free on %s (keeping %s in reserve). "+
			"Exclude what does not belong in the backup, move the repository to another "+
			"volume, or lower the reserve deliberately",
			humanBytes(needed), humanBytes(int64(space.AvailableBytes)), r.Root(),
			humanBytes(opts.ReserveBytes))
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 4; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}
