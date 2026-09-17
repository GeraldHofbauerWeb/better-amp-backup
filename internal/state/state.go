// Package state records what the daemon has done, so that a restart does not
// lose it.
//
// The systemd timer this replaces had Persistent=true: after a reboot it knew
// whether its window had passed. A daemon with the schedule in memory would
// either lose an hour or fire immediately on every restart, and a host that
// reboots in a loop would back up in a loop. The last run's start time is what
// prevents both.
//
// It is separate from settings because nobody edits it: settings is what a
// person decided, state is what happened.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/atomicfile"
)

// Version is the format version of the state file.
const Version = 1

// Kind names the sort of run a Run describes.
type Kind string

const (
	KindBackup       Kind = "backup"
	KindRestore      Kind = "restore"
	KindHousekeeping Kind = "housekeeping"
	KindCheck        Kind = "check"
)

// Run is one completed operation.
type Run struct {
	JobID      string    `json:"job_id"`
	Kind       Kind      `json:"kind"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	// StartedBy is an AMP username, or "scheduler" for an unattended run. It
	// is how the interface and the journal tell a person's click apart from
	// the clock.
	StartedBy string `json:"started_by"`
	Snapshot  string `json:"snapshot,omitempty"`
	Summary   string `json:"summary,omitempty"`
}

// Duration is how long the run took.
func (r Run) Duration() time.Duration { return r.FinishedAt.Sub(r.StartedAt) }

// Skip records a scheduled run that did not happen because something else was
// running. A backup that was due during a three-hour restore is not worth
// taking afterwards -- the next one is an hour away -- but silently missing it
// would leave a gap nobody could explain.
type Skip struct {
	At     time.Time `json:"at"`
	Kind   Kind      `json:"kind"`
	Reason string    `json:"reason"`
}

// State is everything the daemon remembers across a restart.
type State struct {
	Version       int  `json:"version"`
	LastBackup    *Run `json:"last_backup,omitempty"`
	LastRestore   *Run `json:"last_restore,omitempty"`
	LastHousekeep *Run `json:"last_housekeeping,omitempty"`
	LastCheck     *Run `json:"last_check,omitempty"`
	// LastSkip is the most recent scheduled run that was passed over.
	LastSkip *Skip `json:"last_skip,omitempty"`
}

// Store persists State.
type Store struct {
	path string

	mu  sync.RWMutex
	cur State
}

// Open reads the state file, starting empty if there is none.
//
// A state file that cannot be parsed is not fatal. It holds no configuration
// and nothing irreplaceable: refusing to start a backup daemon because its
// bookkeeping is corrupt would turn a cosmetic problem into a missing backup.
func Open(path string) (*Store, error) {
	s := &Store{path: path, cur: State{Version: Version}}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("state: reading %s: %w", path, err)
	}

	var loaded State
	if err := json.Unmarshal(raw, &loaded); err != nil || loaded.Version != Version {
		return s, nil
	}
	s.cur = loaded
	return s, nil
}

// Get returns the current state.
func (s *Store) Get() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Record files a completed run under its kind and persists it.
func (s *Store) Record(run Run) error {
	return s.update(func(st *State) {
		switch run.Kind {
		case KindBackup:
			st.LastBackup = &run
		case KindRestore:
			st.LastRestore = &run
		case KindHousekeeping:
			st.LastHousekeep = &run
		case KindCheck:
			st.LastCheck = &run
		}
	})
}

// RecordSkip files a scheduled run that was passed over.
func (s *Store) RecordSkip(skip Skip) error {
	return s.update(func(st *State) { st.LastSkip = &skip })
}

func (s *Store) update(fn func(*State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.cur
	next.Version = Version
	fn(&next)
	if err := atomicfile.WriteJSON(s.path, next, 0o640); err != nil {
		return err
	}
	s.cur = next
	return nil
}
