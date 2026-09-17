// Package atomicfile writes a file all at once or not at all.
//
// Everything this daemon persists outside the repository -- the schedule, the
// retention policy, the record of the last run -- is small, is read on startup,
// and is rewritten while the thing it configures is running. A half-written
// settings file would take the backup schedule down with it, and a backup tool
// that quietly stops backing up is worse than one that was never installed.
//
// The repository has its own version of this, writing through a temporary
// directory it owns. That one stays where it is: the repository's layout is
// part of its on-disk format, and reaching into it from here would tie the
// schedule's storage to the backup format's.
package atomicfile

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WriteJSON marshals v and installs it at path with the given mode.
//
// The temporary file is created in the target's own directory, because a
// rename is only atomic within one filesystem -- /tmp is routinely a different
// one, and the failure would show up as a corrupt file rather than an error.
func WriteJSON(path string, v any, mode fs.FileMode) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("atomicfile: marshal %s: %w", filepath.Base(path), err)
	}
	return Write(path, append(raw, '\n'), mode)
}

// Write installs raw at path with the given mode.
func Write(path string, raw []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("atomicfile: create %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("atomicfile: temp file next to %s: %w", path, err)
	}
	tmpName := f.Name()
	defer func() {
		if tmpName != "" {
			f.Close()
			os.Remove(tmpName)
		}
	}()

	if _, err := f.Write(raw); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", filepath.Base(path), err)
	}
	// Sync before the rename, or a power loss can leave the directory entry
	// pointing at a file whose contents never reached the disk.
	if err := f.Sync(); err != nil {
		return fmt.Errorf("atomicfile: sync %s: %w", filepath.Base(path), err)
	}
	// CreateTemp makes the file 0600; the mode is set explicitly rather than
	// left to whatever umask happened to be in force.
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("atomicfile: chmod %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("atomicfile: close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomicfile: commit %s: %w", filepath.Base(path), err)
	}
	tmpName = ""
	return nil
}
