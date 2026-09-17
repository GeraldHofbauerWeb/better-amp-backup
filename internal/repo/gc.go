package repo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// Forget removes snapshots from the live set. It never touches objects.
//
// The separation from Prune is the whole safety story: forgetting is a
// metadata edit that a mistaken retention policy can undo, while deleting
// objects is irreversible. Keeping them apart means a bad policy costs a
// restore of two small files rather than the backup itself.
//
// Forgotten snapshots move to snapshots/.trash/ so they can be put back by
// hand until the next prune.
func (r *Repository) Forget(ids []string) error {
	trash := filepath.Join(r.root, dirSnapshots, dirTrash)
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return fmt.Errorf("repo: create trash: %w", err)
	}

	for _, id := range ids {
		manifestPath, err := r.manifestPath(id)
		if err != nil {
			return err
		}
		indexPath, err := r.indexPath(id)
		if err != nil {
			return err
		}
		for _, p := range []string{manifestPath, indexPath} {
			dst := filepath.Join(trash, filepath.Base(p))
			if err := os.Rename(p, dst); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("repo: forget %s: %w", id, err)
			}
		}
	}
	return nil
}

// Restore puts a forgotten snapshot back into the live set.
func (r *Repository) Unforget(id string) error {
	if !ValidSnapshotID(id) {
		return fmt.Errorf("repo: malformed snapshot id %q", id)
	}
	trash := filepath.Join(r.root, dirSnapshots, dirTrash)
	moved := 0
	for _, ext := range []string{manifestExt, indexExt} {
		src := filepath.Join(trash, id+ext)
		dst := filepath.Join(r.root, dirSnapshots, id+ext)
		if err := os.Rename(src, dst); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("repo: unforget %s: %w", id, err)
		}
		moved++
	}
	if moved == 0 {
		return fmt.Errorf("repo: snapshot %s is not in the trash", id)
	}
	return nil
}

// PruneOptions configures garbage collection.
type PruneOptions struct {
	// DryRun reports what would be deleted and deletes nothing.
	DryRun bool

	// GracePeriod protects objects written very recently. A backup writes its
	// objects before it commits its index, so an object younger than this may
	// belong to a run that is still in flight. The repository lock should make
	// that impossible; the grace period is what makes a lock bug survivable.
	GracePeriod time.Duration

	// EmptyTrash also discards forgotten snapshots, which is what actually
	// releases their objects.
	EmptyTrash bool

	Now      func() time.Time
	Progress func(stage string, done int)
}

// DefaultGracePeriod is the age below which an unreferenced object is left
// alone.
const DefaultGracePeriod = time.Hour

// PruneReport is what a prune did, or would have done.
type PruneReport struct {
	LiveSnapshots     int   `json:"live_snapshots"`
	TrashedSnapshots  int   `json:"trashed_snapshots"`
	ReferencedObjects int   `json:"referenced_objects"`
	TotalObjects      int   `json:"total_objects"`
	DeletedObjects    int   `json:"deleted_objects"`
	FreedBytes        int64 `json:"freed_bytes"`
	// Spared counts unreferenced objects left alone because they were younger
	// than the grace period.
	SparedRecent int  `json:"spared_recent"`
	DryRun       bool `json:"dry_run"`
}

// Prune deletes objects that no live snapshot references.
func (r *Repository) Prune(ctx context.Context, opts PruneOptions) (*PruneReport, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.GracePeriod <= 0 {
		opts.GracePeriod = DefaultGracePeriod
	}
	if opts.Progress == nil {
		opts.Progress = func(string, int) {}
	}

	// Exclusive: no backup may add objects while the mark phase decides what
	// is reachable.
	unlock, err := r.LockGC(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if opts.EmptyTrash && !opts.DryRun {
		if err := r.emptyTrash(); err != nil {
			return nil, err
		}
	}

	report := &PruneReport{DryRun: opts.DryRun}

	live, err := r.ListSnapshots()
	if err != nil {
		return nil, err
	}
	report.LiveSnapshots = len(live)

	trashed, err := r.listTrashedIDs()
	if err != nil {
		return nil, err
	}
	report.TrashedSnapshots = len(trashed)

	// Mark. Trashed snapshots still count as reachable unless the trash was
	// emptied, so that Unforget stays possible.
	referenced := make(map[hash.Hash]struct{}, 4096)
	markFrom := func(open func() (*IndexReader, func() error, error), label string) error {
		ir, closeIdx, err := open()
		if err != nil {
			return err
		}
		defer closeIdx()
		for {
			e, err := ir.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return fmt.Errorf("repo: prune: reading %s: %w", label, err)
			}
			if e.Type == TypeFile && !e.Hash.IsZero() {
				referenced[e.Hash] = struct{}{}
			}
		}
	}

	for i, m := range live {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opts.Progress("mark", i)
		id := m.ID
		if err := markFrom(func() (*IndexReader, func() error, error) { return r.OpenIndex(id) }, id); err != nil {
			return nil, err
		}
	}
	if !opts.EmptyTrash {
		for _, id := range trashed {
			p := filepath.Join(r.root, dirSnapshots, dirTrash, id+indexExt)
			if err := markFrom(func() (*IndexReader, func() error, error) {
				return openIndexFile(p)
			}, "trashed "+id); err != nil {
				return nil, err
			}
		}
	}
	report.ReferencedObjects = len(referenced)

	// Sweep.
	cutoff := opts.Now().Add(-opts.GracePeriod).UnixNano()
	var doomed []ObjectInfo
	if err := r.Objects().List(ctx, func(info ObjectInfo) error {
		report.TotalObjects++
		opts.Progress("sweep", report.TotalObjects)
		if _, ok := referenced[info.Hash]; ok {
			return nil
		}
		if info.ModTime > cutoff {
			report.SparedRecent++
			return nil
		}
		doomed = append(doomed, info)
		return nil
	}); err != nil {
		return nil, err
	}

	for _, info := range doomed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		report.DeletedObjects++
		report.FreedBytes += info.StoredSize
		if opts.DryRun {
			continue
		}
		if err := r.Objects().Delete(ctx, info.Hash); err != nil {
			return nil, err
		}
	}
	return report, nil
}

func (r *Repository) listTrashedIDs() ([]string, error) {
	trash := filepath.Join(r.root, dirSnapshots, dirTrash)
	entries, err := os.ReadDir(trash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("repo: list trash: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if len(name) <= len(indexExt) || name[len(name)-len(indexExt):] != indexExt {
			continue
		}
		id := name[:len(name)-len(indexExt)]
		if ValidSnapshotID(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (r *Repository) emptyTrash() error {
	trash := filepath.Join(r.root, dirSnapshots, dirTrash)
	entries, err := os.ReadDir(trash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("repo: empty trash: %w", err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(trash, e.Name())); err != nil {
			return fmt.Errorf("repo: empty trash: %w", err)
		}
	}
	return nil
}

func openIndexFile(path string) (*IndexReader, func() error, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: open index %s: %w", filepath.Base(path), err)
	}
	ir, err := NewIndexReader(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return ir, func() error { ir.Close(); return f.Close() }, nil
}
