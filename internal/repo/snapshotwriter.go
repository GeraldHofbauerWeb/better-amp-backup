package repo

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// SnapshotWriter stages a snapshot and publishes it in one step.
//
// The index is streamed to a temp file while the backup walks the tree, and
// only when Commit succeeds do the index and its manifest appear under
// snapshots/. A crash at any earlier point leaves nothing behind but a temp
// file, never a manifest pointing at an index that does not exist.
type SnapshotWriter struct {
	repo     *Repository
	id       string
	instance string

	tmpIndexPath string
	tmpIndexFile *os.File
	index        *IndexWriter

	done bool
}

// NewSnapshotWriter begins a snapshot. root is the absolute directory that
// entry paths are relative to.
func (r *Repository) NewSnapshotWriter(id, instance, root string) (*SnapshotWriter, error) {
	if !ValidSnapshotID(id) {
		return nil, fmt.Errorf("repo: malformed snapshot id %q", id)
	}
	if instance == "" {
		return nil, fmt.Errorf("repo: snapshot needs an instance name")
	}
	if p, err := r.manifestPath(id); err == nil {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("repo: snapshot %s already exists", id)
		}
	}

	f, err := os.CreateTemp(filepath.Join(r.root, dirTmp), "idx-*")
	if err != nil {
		return nil, fmt.Errorf("repo: temp index: %w", err)
	}
	iw, err := NewIndexWriter(f, id, instance, root)
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &SnapshotWriter{
		repo: r, id: id, instance: instance,
		tmpIndexPath: f.Name(), tmpIndexFile: f, index: iw,
	}, nil
}

// ID returns the snapshot ID being written.
func (w *SnapshotWriter) ID() string { return w.id }

// Add records one filesystem entry.
func (w *SnapshotWriter) Add(e Entry) error {
	if w.done {
		return fmt.Errorf("repo: snapshot %s is already finished", w.id)
	}
	return w.index.Add(e)
}

// Entries reports how many entries have been staged.
func (w *SnapshotWriter) Entries() int64 { return w.index.Entries() }

// Commit publishes the snapshot. The manifest's ID, IndexHash and Version are
// filled in here; everything else is the caller's.
//
// The index is renamed into place first: if the process dies between the two
// renames the orphaned index is harmless (nothing references it) and gets
// cleaned up, whereas a manifest without an index would be a broken snapshot.
func (w *SnapshotWriter) Commit(m *Manifest) error {
	if w.done {
		return fmt.Errorf("repo: snapshot %s is already finished", w.id)
	}
	w.done = true

	indexHash, err := w.index.Close()
	if err != nil {
		w.cleanupTemp()
		return err
	}
	if err := w.tmpIndexFile.Sync(); err != nil {
		w.cleanupTemp()
		return fmt.Errorf("repo: sync index: %w", err)
	}
	if err := w.tmpIndexFile.Close(); err != nil {
		w.cleanupTemp()
		return fmt.Errorf("repo: close index: %w", err)
	}

	finalIndex, err := w.repo.indexPath(w.id)
	if err != nil {
		w.cleanupTemp()
		return err
	}
	if err := os.Rename(w.tmpIndexPath, finalIndex); err != nil {
		w.cleanupTemp()
		return fmt.Errorf("repo: commit index %s: %w", w.id, err)
	}
	w.tmpIndexPath = ""

	m.Version = manifestVersion
	m.ID = w.id
	m.Instance = w.instance
	m.IndexHash = indexHash
	if m.State == "" {
		m.State = StateComplete
	}
	if err := m.Validate(); err != nil {
		os.Remove(finalIndex)
		return err
	}

	finalManifest, err := w.repo.manifestPath(w.id)
	if err != nil {
		os.Remove(finalIndex)
		return err
	}
	if err := writeJSONAtomic(finalManifest, filepath.Join(w.repo.root, dirTmp), m); err != nil {
		os.Remove(finalIndex)
		return err
	}
	return nil
}

// Abort discards a snapshot in progress. It is safe to call after Commit.
func (w *SnapshotWriter) Abort() error {
	if w.done {
		return nil
	}
	w.done = true
	w.index.Close()
	w.cleanupTemp()
	return nil
}

func (w *SnapshotWriter) cleanupTemp() {
	if w.tmpIndexFile != nil {
		w.tmpIndexFile.Close()
	}
	if w.tmpIndexPath != "" {
		os.Remove(w.tmpIndexPath)
		w.tmpIndexPath = ""
	}
}

// VerifyIndexHash re-reads a snapshot's index file and checks it against the
// hash its manifest recorded. This is stage one of `amp-bb check`.
func (r *Repository) VerifyIndexHash(m Manifest) error {
	p, err := r.indexPath(m.ID)
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("repo: open index %s: %w", m.ID, err)
	}
	defer f.Close()

	got, _, err := hash.OfReader(f)
	if err != nil {
		return fmt.Errorf("repo: read index %s: %w", m.ID, err)
	}
	if got != m.IndexHash {
		return fmt.Errorf("repo: index of %s is corrupted (hash %s, manifest says %s)",
			m.ID, got, m.IndexHash)
	}
	return nil
}
