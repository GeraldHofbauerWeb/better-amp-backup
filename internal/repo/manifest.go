package repo

import (
	"fmt"
	"regexp"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/hash"
)

// SnapshotState records how a backup run ended.
type SnapshotState string

const (
	// StateComplete means every planned path was captured.
	StateComplete SnapshotState = "complete"
	// StatePartial means the snapshot is usable but something was skipped or
	// read while it was still changing. Restores from it are allowed but warn.
	StatePartial SnapshotState = "partial"
	// StateFailed means the run aborted. Such snapshots are never restored
	// from and are cleaned up by the next run.
	StateFailed SnapshotState = "failed"
)

// Stats is the accounting a run reports, and the raw material for the churn
// analysis that tells a user which directories actually cost them space.
type Stats struct {
	Files    int64 `json:"files"`
	Dirs     int64 `json:"dirs"`
	Symlinks int64 `json:"symlinks"`

	// TotalBytes is the plaintext size of everything the snapshot references.
	TotalBytes int64 `json:"total_bytes"`

	// NewObjects and NewBytes are what this run actually added to the
	// repository. On a healthy incremental run these are near zero.
	NewObjects int64 `json:"new_objects"`
	NewBytes   int64 `json:"new_bytes"`

	// ReusedObjects counts the dedup hits.
	ReusedObjects int64 `json:"reused_objects"`

	// UnchangedFiles counts files the stat-diff let us skip reading entirely.
	UnchangedFiles int64 `json:"unchanged_files"`

	// RereadFiles counts files that changed under us mid-read and had to be
	// read again. A persistently non-zero value means quiescing is not working.
	RereadFiles int64 `json:"reread_files"`
}

// Manifest is the small, quickly listable description of a snapshot. It sits
// beside the index so that `snapshots` never has to decompress anything.
type Manifest struct {
	Version int    `json:"version"`
	ID      string `json:"id"`

	Instance   string `json:"instance"`
	InstanceID string `json:"instance_id,omitempty"`
	Root       string `json:"root"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	// QuiesceMillis is how long the application was held with saves disabled.
	// This is the number that has to stay small; it is surfaced by `stats` and
	// checked by the acceptance criteria.
	QuiesceMillis int64 `json:"quiesce_millis"`

	// Parent is the snapshot this run diffed against, empty for a first run.
	Parent string `json:"parent,omitempty"`

	State SnapshotState `json:"state"`
	Tags  []string      `json:"tags,omitempty"`

	Stats Stats `json:"stats"`

	// IndexHash covers the compressed index bytes, so a truncated or swapped
	// index is detectable without reading it.
	IndexHash hash.Hash `json:"index_hash"`

	ToolVersion string `json:"tool_version"`

	// Warnings carries anything the operator should see but that did not
	// invalidate the snapshot, e.g. a file that vanished mid-run.
	Warnings []string `json:"warnings,omitempty"`
}

const manifestVersion = 1

// snapshotIDPattern is "<RFC3339-ish compact UTC>-<6 hex>", e.g.
// 20260907T170000Z-3f2a1b. Sorting these lexically sorts them by time, which
// is what makes retention and `latest` cheap.
var snapshotIDPattern = regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{6}$`)

// NewSnapshotID builds an ID for a run started at t, using suffix for
// uniqueness within the same second.
func NewSnapshotID(t time.Time, suffix string) string {
	return fmt.Sprintf("%s-%s", t.UTC().Format("20060102T150405Z"), suffix)
}

// ValidSnapshotID reports whether s is a well-formed snapshot ID. Anything
// reaching the filesystem as a path component is checked with this first, so
// that a crafted ID cannot escape the snapshots directory.
func ValidSnapshotID(s string) bool { return snapshotIDPattern.MatchString(s) }

// Duration is how long the whole run took.
func (m Manifest) Duration() time.Duration { return m.FinishedAt.Sub(m.StartedAt) }

// Restorable reports whether this snapshot may be used as a restore source.
func (m Manifest) Restorable() bool {
	return m.State == StateComplete || m.State == StatePartial
}

// HasTag reports whether the snapshot carries the given tag.
func (m Manifest) HasTag(tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Validate checks a manifest read from disk.
func (m Manifest) Validate() error {
	if m.Version != manifestVersion {
		return fmt.Errorf("manifest: unsupported version %d (this build reads %d)",
			m.Version, manifestVersion)
	}
	if !ValidSnapshotID(m.ID) {
		return fmt.Errorf("manifest: malformed snapshot id %q", m.ID)
	}
	if m.Instance == "" {
		return fmt.Errorf("manifest %s: no instance recorded", m.ID)
	}
	switch m.State {
	case StateComplete, StatePartial, StateFailed:
	default:
		return fmt.Errorf("manifest %s: unknown state %q", m.ID, m.State)
	}
	if m.Parent != "" && !ValidSnapshotID(m.Parent) {
		return fmt.Errorf("manifest %s: malformed parent id %q", m.ID, m.Parent)
	}
	return nil
}
