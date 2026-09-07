// Package repo implements the content-addressed backup repository: the object
// store, the snapshot index format, and the locking that keeps concurrent
// backups and garbage collection from stepping on each other.
package repo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	// ConfigName is the marker that makes a directory a repository.
	ConfigName = "config.json"

	dirObjects   = "objects"
	dirSnapshots = "snapshots"
	dirInstances = "instances"
	dirLocks     = "locks"
	dirTmp       = "tmp"
	dirTrash     = ".trash"

	manifestExt = ".json"
	indexExt    = ".idx.zst"

	repoFormatVersion = 1
)

// Config is the on-disk description of a repository. It is written once at
// init and read on every open, so that a future format change is detected
// rather than silently mis-parsed.
type Config struct {
	Version          int       `json:"version"`
	ID               string    `json:"id"`
	CreatedAt        time.Time `json:"created_at"`
	HashAlgo         string    `json:"hash_algo"`
	CompressionAlgo  string    `json:"compression_algo"`
	CompressionLevel int       `json:"compression_level"`
	// Chunking is recorded even though v1 always stores whole files, so that a
	// repository written by a future chunking build is recognisable.
	Chunking string `json:"chunking"`
}

// Repository is an opened backup repository.
type Repository struct {
	root    string
	cfg     Config
	objects *FSObjectStore
}

// Init creates a new repository at root. It refuses to touch a directory that
// already holds one.
func Init(root string, compressionLevel int) (*Repository, error) {
	if _, err := os.Stat(filepath.Join(root, ConfigName)); err == nil {
		return nil, fmt.Errorf("repo: %s already holds a repository", root)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("repo: stat %s: %w", root, err)
	}

	for _, d := range []string{dirObjects, dirSnapshots, dirInstances, dirLocks, dirTmp,
		filepath.Join(dirSnapshots, dirTrash)} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("repo: create %s: %w", d, err)
		}
	}

	cfg := Config{
		Version:          repoFormatVersion,
		ID:               newRepoID(),
		CreatedAt:        time.Now().UTC(),
		HashAlgo:         "blake3-256",
		CompressionAlgo:  "zstd",
		CompressionLevel: compressionLevel,
		Chunking:         "whole-file",
	}
	if err := writeJSONAtomic(filepath.Join(root, ConfigName), filepath.Join(root, dirTmp), cfg); err != nil {
		return nil, err
	}
	return Open(root)
}

// Open loads an existing repository.
func Open(root string) (*Repository, error) {
	raw, err := os.ReadFile(filepath.Join(root, ConfigName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("repo: no repository at %s (run `amp-bb init` first)", root)
		}
		return nil, fmt.Errorf("repo: read config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("repo: parse config: %w", err)
	}
	if cfg.Version != repoFormatVersion {
		return nil, fmt.Errorf("repo: format version %d is not supported by this build (expects %d)",
			cfg.Version, repoFormatVersion)
	}
	if cfg.HashAlgo != "blake3-256" {
		return nil, fmt.Errorf("repo: unsupported hash algorithm %q", cfg.HashAlgo)
	}

	// Directories may be missing if a repository was copied selectively.
	for _, d := range []string{dirObjects, dirSnapshots, dirInstances, dirLocks, dirTmp,
		filepath.Join(dirSnapshots, dirTrash)} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, fmt.Errorf("repo: create %s: %w", d, err)
		}
	}

	objects, err := NewFSObjectStore(
		filepath.Join(root, dirObjects), filepath.Join(root, dirTmp), cfg.CompressionLevel)
	if err != nil {
		return nil, err
	}
	return &Repository{root: root, cfg: cfg, objects: objects}, nil
}

// Root returns the repository directory.
func (r *Repository) Root() string { return r.root }

// Config returns the repository configuration.
func (r *Repository) Config() Config { return r.cfg }

// Objects returns the object store.
func (r *Repository) Objects() ObjectStore { return r.objects }

// LockWrite takes the shared lock held for the duration of a backup. Several
// backups may hold it at once; garbage collection may not run while it is held.
func (r *Repository) LockWrite(ctx context.Context) (func() error, error) {
	return r.lock(ctx, "repo.write", false)
}

// LockGC takes the exclusive lock that garbage collection needs. It waits for
// in-flight backups to finish rather than racing them.
func (r *Repository) LockGC(ctx context.Context) (func() error, error) {
	return r.lock(ctx, "repo.gc", true)
}

func (r *Repository) lock(ctx context.Context, name string, exclusive bool) (func() error, error) {
	fl := flock.New(filepath.Join(r.root, dirLocks, name+".lock"))
	var ok bool
	var err error
	if exclusive {
		ok, err = fl.TryLockContext(ctx, 250*time.Millisecond)
	} else {
		ok, err = fl.TryRLockContext(ctx, 250*time.Millisecond)
	}
	if err != nil {
		return nil, fmt.Errorf("repo: acquire %s lock: %w", name, err)
	}
	if !ok {
		return nil, fmt.Errorf("repo: %s lock is held by another process", name)
	}
	return fl.Unlock, nil
}

// manifestPath and indexPath validate the ID first, so that a value read from
// a manifest or the command line can never escape the snapshots directory.
func (r *Repository) manifestPath(id string) (string, error) {
	if !ValidSnapshotID(id) {
		return "", fmt.Errorf("repo: malformed snapshot id %q", id)
	}
	return filepath.Join(r.root, dirSnapshots, id+manifestExt), nil
}

func (r *Repository) indexPath(id string) (string, error) {
	if !ValidSnapshotID(id) {
		return "", fmt.Errorf("repo: malformed snapshot id %q", id)
	}
	return filepath.Join(r.root, dirSnapshots, id+indexExt), nil
}

// LoadManifest reads a single manifest.
func (r *Repository) LoadManifest(id string) (Manifest, error) {
	p, err := r.manifestPath(id)
	if err != nil {
		return Manifest{}, err
	}
	return readManifest(p)
}

func readManifest(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("repo: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("repo: parse manifest %s: %w", filepath.Base(path), err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// ListSnapshots returns every manifest, oldest first. Snapshot IDs sort
// lexically by time, so this needs no date parsing.
func (r *Repository) ListSnapshots() ([]Manifest, error) {
	entries, err := os.ReadDir(filepath.Join(r.root, dirSnapshots))
	if err != nil {
		return nil, fmt.Errorf("repo: list snapshots: %w", err)
	}
	var out []Manifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), manifestExt) {
			continue
		}
		m, err := readManifest(filepath.Join(r.root, dirSnapshots, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// OpenIndex streams the index of a snapshot. The caller must Close it.
func (r *Repository) OpenIndex(id string) (*IndexReader, func() error, error) {
	p, err := r.indexPath(id)
	if err != nil {
		return nil, nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, nil, fmt.Errorf("repo: open index %s: %w", id, err)
	}
	ir, err := NewIndexReader(f)
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return ir, func() error { ir.Close(); return f.Close() }, nil
}

// LatestFor returns the most recent complete snapshot ID for an instance, or
// "" when there is none. This is the parent a stat-diff runs against.
func (r *Repository) LatestFor(instance string) (string, error) {
	all, err := r.ListSnapshots()
	if err != nil {
		return "", err
	}
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Instance == instance && all[i].State == StateComplete {
			return all[i].ID, nil
		}
	}
	return "", nil
}

func newRepoID() string {
	return fmt.Sprintf("%x", time.Now().UTC().UnixNano())
}

// writeJSONAtomic serialises v into path via a temp file and a rename, so a
// reader never observes a half-written file.
func writeJSONAtomic(path, tmpDir string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("repo: marshal %s: %w", filepath.Base(path), err)
	}
	raw = append(raw, '\n')

	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("repo: create tmp dir: %w", err)
	}
	f, err := os.CreateTemp(tmpDir, "json-*")
	if err != nil {
		return fmt.Errorf("repo: temp file: %w", err)
	}
	tmpName := f.Name()
	defer func() {
		if tmpName != "" {
			f.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("repo: write %s: %w", filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("repo: sync %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("repo: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("repo: commit %s: %w", filepath.Base(path), err)
	}
	tmpName = ""
	return nil
}
