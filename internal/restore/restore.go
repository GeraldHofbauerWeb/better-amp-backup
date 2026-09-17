// Package restore writes a snapshot back out to a directory.
//
// Every file written is re-hashed as it goes and checked against what the
// index promised. A backup tool that can silently restore corrupted bytes is
// worse than no backup tool, so a mismatch aborts the restore rather than
// leaving a plausible-looking but wrong world on disk.
package restore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// Options configures a restore into a target directory.
type Options struct {
	// Snapshot is the ID to restore.
	Snapshot string
	// Target is the directory to write into. It is created if missing.
	Target string

	// Include, when set, restricts the restore to matching paths.
	Include *exclude.Set

	// IncludePaths, when non-empty, restricts the restore to these exact
	// snapshot-relative paths and everything beneath each of them.
	//
	// This is deliberately not Include. A checked box in a file browser is a
	// path, not a pattern, and a filename holding a glob metacharacter -- a
	// modpack is full of them -- cannot survive the round trip through one.
	//
	// Setting both this and Include is an error rather than a union: two
	// filters that disagree is a bug report waiting to happen.
	IncludePaths []string

	// DryRun plans the restore and reports it without touching the target.
	DryRun bool

	// Overwrite allows writing into a target that is not empty.
	Overwrite bool

	// RestoreTimes sets modification times to match the snapshot. It is on by
	// default via NewOptions; tests that compare trees may want it off.
	RestoreTimes bool

	Progress func(done, total int)
}

// Report is what a restore produced.
type Report struct {
	Files    int
	Dirs     int
	Symlinks int
	Bytes    int64
	// Verified counts files whose content hash matched the index.
	Verified int
	Skipped  int
	DryRun   bool

	// Warnings carries what the operator has to know but that did not stop the
	// restore -- so far, only that the source snapshot was partial. A library
	// reports such a thing; it does not write to stderr, because under the
	// daemon stderr is the journal and nobody watching a restore in a browser
	// will ever look there.
	Warnings []string
}

// Run restores a snapshot.
func Run(ctx context.Context, r *repo.Repository, opts Options) (*Report, error) {
	if opts.Target == "" {
		return nil, errors.New("restore: no target directory")
	}
	if opts.Progress == nil {
		opts.Progress = func(int, int) {}
	}

	if opts.Include != nil && len(opts.IncludePaths) > 0 {
		return nil, errors.New("restore: pass either Include or IncludePaths, not both")
	}
	sel, err := newSelection(opts.IncludePaths)
	if err != nil {
		return nil, err
	}

	m, err := r.LoadManifest(opts.Snapshot)
	if err != nil {
		return nil, err
	}
	if !m.Restorable() {
		return nil, fmt.Errorf("restore: snapshot %s is in state %q and must not be restored",
			m.ID, m.State)
	}
	target, err := filepath.Abs(opts.Target)
	if err != nil {
		return nil, fmt.Errorf("restore: resolve target: %w", err)
	}
	if err := checkTarget(target, opts); err != nil {
		return nil, err
	}

	entries, err := collect(r, m.ID, opts.Include, sel)
	if err != nil {
		return nil, err
	}
	// A selection that matched nothing would restore nothing and say it
	// succeeded, which is the worst outcome this command has.
	if sel != nil {
		if missing := sel.unmatched(); len(missing) > 0 {
			return nil, fmt.Errorf("restore: snapshot %s holds no %s",
				m.ID, strings.Join(missing, ", "))
		}
	}

	rep := &Report{DryRun: opts.DryRun}
	if m.State == repo.StatePartial {
		// Not fatal, but the operator has to know the source was imperfect --
		// and has to know it even for a dry run, which is where someone looks
		// before committing to the real thing.
		rep.Warnings = append(rep.Warnings, fmt.Sprintf(
			"snapshot %s is partial (%d warnings recorded when it was taken)",
			m.ID, len(m.Warnings)))
	}
	if opts.DryRun {
		for _, e := range entries {
			countEntry(rep, e)
		}
		return rep, nil
	}

	if err := os.MkdirAll(target, 0o755); err != nil {
		return nil, fmt.Errorf("restore: create target: %w", err)
	}

	// Directories first, so parents exist before their children.
	for _, e := range entries {
		if e.Type != repo.TypeDir {
			continue
		}
		dst, err := safeJoin(target, e.Path)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dst, e.Mode.Perm()|0o700); err != nil {
			return nil, fmt.Errorf("restore: create %s: %w", e.Path, err)
		}
		rep.Dirs++
	}

	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opts.Progress(i, len(entries))

		switch e.Type {
		case repo.TypeDir:
			continue
		case repo.TypeSymlink:
			if err := writeSymlink(target, e); err != nil {
				return nil, err
			}
			rep.Symlinks++
		case repo.TypeFile:
			n, err := writeFile(ctx, r, target, e)
			if err != nil {
				return nil, err
			}
			rep.Files++
			rep.Verified++
			rep.Bytes += n
		}
	}
	opts.Progress(len(entries), len(entries))

	if opts.RestoreTimes {
		if err := applyTimes(target, entries); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

func countEntry(rep *Report, e repo.Entry) {
	switch e.Type {
	case repo.TypeDir:
		rep.Dirs++
	case repo.TypeSymlink:
		rep.Symlinks++
	case repo.TypeFile:
		rep.Files++
		rep.Bytes += e.Size
	}
}

func collect(r *repo.Repository, id string, include *exclude.Set, sel *selection) ([]repo.Entry, error) {
	ir, closeIdx, err := r.OpenIndex(id)
	if err != nil {
		return nil, err
	}
	defer closeIdx()

	var out []repo.Entry
	for {
		e, err := ir.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if include != nil && !include.Match(e.Path) {
			continue
		}
		if sel != nil && !sel.keep(e) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func checkTarget(target string, opts Options) error {
	if opts.DryRun || opts.Overwrite {
		return nil
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("restore: inspect target: %w", err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("restore: target %s is not empty (pass Overwrite to write into it anyway)",
			target)
	}
	return nil
}

// safeJoin resolves a snapshot-relative path inside target, refusing anything
// that would escape it. Entry.Validate already rejects such paths, but a
// restore writes to the filesystem and is worth checking twice.
func safeJoin(target, rel string) (string, error) {
	joined := filepath.Join(target, filepath.FromSlash(rel))
	cleanTarget := filepath.Clean(target) + string(filepath.Separator)
	if joined != filepath.Clean(target) && !hasPrefix(joined, cleanTarget) {
		return "", fmt.Errorf("restore: path %q escapes the target directory", rel)
	}
	return joined, nil
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func writeSymlink(target string, e repo.Entry) error {
	dst, err := safeJoin(target, e.Path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("restore: create parent of %s: %w", e.Path, err)
	}
	// Replace whatever is there; a stale link would otherwise survive.
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore: replace %s: %w", e.Path, err)
	}
	if err := os.Symlink(e.Target, dst); err != nil {
		return fmt.Errorf("restore: link %s: %w", e.Path, err)
	}
	return nil
}

// writeFile streams one object out of the repository, verifying its hash as it
// writes, and only then puts it in place.
func writeFile(ctx context.Context, r *repo.Repository, target string, e repo.Entry) (int64, error) {
	dst, err := safeJoin(target, e.Path)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("restore: create parent of %s: %w", e.Path, err)
	}

	// An empty file has no object behind it; write it directly.
	if e.Hash.IsZero() {
		if e.Size != 0 {
			return 0, fmt.Errorf("restore: %s has no content hash but claims %d bytes",
				e.Path, e.Size)
		}
		if err := os.WriteFile(dst, nil, e.Mode.Perm()); err != nil {
			return 0, fmt.Errorf("restore: write %s: %w", e.Path, err)
		}
		return 0, nil
	}

	rc, err := r.Objects().Open(ctx, e.Hash)
	if err != nil {
		return 0, fmt.Errorf("restore: %s: %w", e.Path, err)
	}
	defer rc.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".amp-bb-restore-*")
	if err != nil {
		return 0, fmt.Errorf("restore: temp file for %s: %w", e.Path, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()

	hasher := hash.New()
	n, err := io.Copy(io.MultiWriter(tmp, hasher), rc)
	if err != nil {
		return 0, fmt.Errorf("restore: read %s: %w", e.Path, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("restore: close %s: %w", e.Path, err)
	}

	if got := hasher.Sum(); got != e.Hash {
		return 0, fmt.Errorf("restore: %s is corrupted in the repository "+
			"(content hashes to %s, index says %s); nothing was written", e.Path, got, e.Hash)
	}
	if n != e.Size {
		return 0, fmt.Errorf("restore: %s is %d bytes, index says %d", e.Path, n, e.Size)
	}

	if err := os.Chmod(tmpName, e.Mode.Perm()); err != nil {
		return 0, fmt.Errorf("restore: chmod %s: %w", e.Path, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return 0, fmt.Errorf("restore: place %s: %w", e.Path, err)
	}
	tmpName = ""
	return n, nil
}

// applyTimes walks the entries deepest-first, so that writing a child does not
// reset the timestamp of a parent that was already fixed up.
func applyTimes(target string, entries []repo.Entry) error {
	ordered := append([]repo.Entry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path > ordered[j].Path })

	for _, e := range ordered {
		if e.ModTime == 0 || e.Type == repo.TypeSymlink {
			continue
		}
		dst, err := safeJoin(target, e.Path)
		if err != nil {
			return err
		}
		mt := time.Unix(0, e.ModTime)
		if err := os.Chtimes(dst, mt, mt); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("restore: set times on %s: %w", e.Path, err)
		}
	}
	return nil
}

// Verify walks a restored tree and compares it against the snapshot, reporting
// every difference. This is what turns "the restore ran" into "the restore is
// provably identical to what was backed up".
func Verify(ctx context.Context, r *repo.Repository, snapshot, target string) ([]string, error) {
	entries, err := collect(r, snapshot, nil, nil)
	if err != nil {
		return nil, err
	}
	var diffs []string
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dst, err := safeJoin(target, e.Path)
		if err != nil {
			return nil, err
		}
		info, err := os.Lstat(dst)
		if err != nil {
			diffs = append(diffs, fmt.Sprintf("%s: missing from restore", e.Path))
			continue
		}

		switch e.Type {
		case repo.TypeDir:
			if !info.IsDir() {
				diffs = append(diffs, fmt.Sprintf("%s: expected a directory", e.Path))
			}
		case repo.TypeSymlink:
			if info.Mode()&fs.ModeSymlink == 0 {
				diffs = append(diffs, fmt.Sprintf("%s: expected a symlink", e.Path))
				continue
			}
			got, lerr := os.Readlink(dst)
			if lerr != nil || got != e.Target {
				diffs = append(diffs, fmt.Sprintf("%s: link points at %q, want %q", e.Path, got, e.Target))
			}
		case repo.TypeFile:
			if info.Size() != e.Size {
				diffs = append(diffs, fmt.Sprintf("%s: %d bytes on disk, %d in snapshot",
					e.Path, info.Size(), e.Size))
				continue
			}
			f, oerr := os.Open(dst)
			if oerr != nil {
				diffs = append(diffs, fmt.Sprintf("%s: %v", e.Path, oerr))
				continue
			}
			got, _, herr := hash.OfReader(f)
			f.Close()
			if herr != nil {
				diffs = append(diffs, fmt.Sprintf("%s: %v", e.Path, herr))
				continue
			}
			if !e.Hash.IsZero() && got != e.Hash {
				diffs = append(diffs, fmt.Sprintf("%s: content differs", e.Path))
			}
		}
		if e.Type != repo.TypeSymlink && info.Mode().Perm() != e.Mode.Perm() {
			diffs = append(diffs, fmt.Sprintf("%s: mode %o on disk, %o in snapshot",
				e.Path, info.Mode().Perm(), e.Mode.Perm()))
		}
	}
	return diffs, nil
}
