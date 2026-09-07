// Package scan walks an AMP instance directory and decides, per file, whether
// the previous snapshot already holds its contents.
//
// This is where the incremental behaviour comes from. AMP re-reads and
// re-compresses all 13.4 GB of an instance every hour; a stat-diff against the
// previous index reduces that to the handful of region files a player actually
// touched, which is what allows the read to happen inside a sub-second quiesce
// window instead of a multi-minute one.
package scan

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// Tier says whether a path has to be read while the application is quiesced.
type Tier int

const (
	// Cold paths can be read while the server is running: mods, configs, jars.
	// A torn read there means a stale copy of a file nobody was writing.
	Cold Tier = iota
	// Hot paths are the ones the server rewrites in place — region files,
	// level.dat, playerdata. These are read only between save-off and save-on.
	Hot
)

func (t Tier) String() string {
	if t == Hot {
		return "hot"
	}
	return "cold"
}

// Item is one filesystem entry found by the walk.
type Item struct {
	// Path is relative to the root, slash-separated.
	Path string
	// Abs is the absolute path to open.
	Abs string

	Type    repo.EntryType
	Mode    fs.FileMode
	UID     int
	GID     int
	Size    int64
	ModTime int64
	Inode   uint64

	// Target is set for symlinks.
	Target string

	Tier Tier
}

// Entry converts an Item into the index entry that describes it, leaving the
// content hash to the caller.
func (it Item) Entry() repo.Entry {
	return repo.Entry{
		Path:    it.Path,
		Type:    it.Type,
		Mode:    it.Mode,
		UID:     it.UID,
		GID:     it.GID,
		Size:    it.Size,
		ModTime: it.ModTime,
		Inode:   it.Inode,
		Target:  it.Target,
	}
}

// Options configures a walk.
type Options struct {
	// Root is the absolute directory to walk.
	Root string
	// Exclude decides which paths are skipped entirely.
	Exclude *exclude.Set
	// Hot marks the paths that must be read under quiesce.
	Hot *exclude.Set
	// SkipAbs holds absolute paths that must never be descended into, used to
	// keep a repository stored inside the instance from backing up itself.
	SkipAbs []string
}

// Result is what a walk produced.
type Result struct {
	Items []Item
	// Warnings records paths that could not be read. They do not fail the run,
	// but they do downgrade the snapshot to "partial".
	Warnings []string
	// Excluded counts paths skipped by the exclusion rules, for reporting.
	Excluded int64
	// ExcludedBytes is the plaintext size the exclusions saved.
	ExcludedBytes int64
}

// Walk collects every entry under opts.Root.
//
// Symlinks are recorded as links and never followed: following them could
// escape the instance directory entirely, and AMP itself stores them verbatim.
func Walk(ctx context.Context, opts Options) (*Result, error) {
	if opts.Root == "" {
		return nil, fmt.Errorf("scan: no root given")
	}
	root, err := filepath.Abs(opts.Root)
	if err != nil {
		return nil, fmt.Errorf("scan: resolve root: %w", err)
	}

	skip := make(map[string]bool, len(opts.SkipAbs))
	for _, p := range opts.SkipAbs {
		if abs, err := filepath.Abs(p); err == nil {
			skip[abs] = true
		}
	}

	res := &Result{}
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			// A file that vanished between readdir and stat is normal on a live
			// server. Note it and carry on rather than failing the whole run.
			if os.IsNotExist(err) || os.IsPermission(err) {
				res.Warnings = append(res.Warnings,
					fmt.Sprintf("%s: %v", relOrAbs(root, abs), err))
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}

		if skip[abs] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, rerr := filepath.Rel(root, abs)
		if rerr != nil {
			return fmt.Errorf("scan: relativise %s: %w", abs, rerr)
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil // the root itself is implied
		}

		if opts.Exclude != nil && opts.Exclude.Match(rel) {
			res.Excluded++
			if info, ierr := d.Info(); ierr == nil && !d.IsDir() {
				res.ExcludedBytes += info.Size()
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			if os.IsNotExist(ierr) {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: vanished during scan", rel))
				return nil
			}
			return fmt.Errorf("scan: stat %s: %w", rel, ierr)
		}

		it := Item{
			Path:    rel,
			Abs:     abs,
			Mode:    info.Mode().Perm(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			it.UID = int(st.Uid)
			it.GID = int(st.Gid)
			it.Inode = st.Ino
		}

		switch {
		case d.IsDir():
			it.Type = repo.TypeDir
			it.Size = 0
		case info.Mode()&fs.ModeSymlink != 0:
			it.Type = repo.TypeSymlink
			target, lerr := os.Readlink(abs)
			if lerr != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s: unreadable symlink: %v", rel, lerr))
				return nil
			}
			it.Target = target
			it.Size = 0
		case info.Mode().IsRegular():
			it.Type = repo.TypeFile
		default:
			// Sockets, devices and FIFOs have no meaningful backup
			// representation and restoring them could be actively harmful.
			res.Warnings = append(res.Warnings,
				fmt.Sprintf("%s: skipped, not a regular file (%s)", rel, info.Mode().Type()))
			return nil
		}

		if opts.Hot != nil && opts.Hot.Match(rel) {
			it.Tier = Hot
		}
		res.Items = append(res.Items, it)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A stable order makes snapshots reproducible and diffs readable.
	sort.Slice(res.Items, func(i, j int) bool { return res.Items[i].Path < res.Items[j].Path })
	return res, nil
}

// Unchanged reports whether prev already describes the same content as it.
//
// The comparison is deliberately conservative: any doubt resolves to "changed",
// because a false "changed" costs one re-read while a false "unchanged" means
// a modification is never backed up.
func Unchanged(prev repo.Entry, it Item) bool {
	if prev.Type != it.Type || prev.Hash.IsZero() {
		return false
	}
	if prev.Size != it.Size || prev.ModTime != it.ModTime {
		return false
	}
	if prev.Mode != it.Mode {
		return false
	}
	// Inode is only compared when both sides recorded one, so that an index
	// written before inodes were tracked does not force a full re-read.
	if prev.Inode != 0 && it.Inode != 0 && prev.Inode != it.Inode {
		return false
	}
	return true
}

// Split partitions items by tier, preserving order within each.
func Split(items []Item) (cold, hot []Item) {
	for _, it := range items {
		if it.Tier == Hot {
			hot = append(hot, it)
		} else {
			cold = append(cold, it)
		}
	}
	return cold, hot
}

func relOrAbs(root, abs string) string {
	if rel, err := filepath.Rel(root, abs); err == nil {
		return filepath.ToSlash(rel)
	}
	return strings.TrimPrefix(abs, root)
}
