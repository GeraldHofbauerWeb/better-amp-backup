// Package settings holds the daemon's persisted, user-editable configuration.
//
// It exists because the schedule has to be changeable from a browser. The
// daemon runs as the amp user and cannot rewrite a systemd timer or reload
// systemd, so it keeps the cadence itself, here, in a file it owns.
//
// This is the only state the web interface may change, and it is written
// atomically: a half-written settings file would take the backup schedule down
// with it, and a backup tool that quietly stops backing up is worse than one
// that was never installed.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/atomicfile"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
)

// Version is the format version of the settings file.
const Version = 1

// File is the whole of the persisted configuration.
type File struct {
	Version    int        `json:"version"`
	Instance   Instance   `json:"instance"`
	Schedule   Schedule   `json:"schedule"`
	Retention  Retention  `json:"retention"`
	Exclusions Exclusions `json:"exclusions"`

	UpdatedAt time.Time `json:"updated_at"`
	// UpdatedBy is the AMP username that last wrote this, or "install" for the
	// file the daemon created on first start. Settings that decide which
	// snapshots get deleted deserve an author.
	UpdatedBy string `json:"updated_by"`
}

// Instance identifies what is being backed up. It is set once, from the
// command line, and is not editable in the browser: a target path arriving
// over HTTP is a write primitive, not a setting.
type Instance struct {
	Name string `json:"name"`
	// ID is the AMP instance GUID, used to reach it through the controller's
	// proxy. Empty is allowed; only the panel-session path needs it.
	ID   string `json:"id"`
	Root string `json:"root"`
}

// Schedule is how often a backup runs.
type Schedule struct {
	Enabled bool `json:"enabled"`
	// Every is the interval between backups, as a Go duration string.
	Every string `json:"every"`
	// Jitter randomises each fire within [0, Jitter), the way the systemd
	// timer's RandomizedDelaySec did and for the same reason: AMP's own backup
	// runs on the hour, and two backups of one instance at once is the worst
	// possible overlap.
	Jitter  string `json:"jitter"`
	Quiesce bool   `json:"quiesce"`
	// StartupGrace delays the first fire after the daemon starts, so that a
	// host reboot does not back up an instance AMP is still bringing up.
	StartupGrace string `json:"startup_grace"`

	Housekeeping Housekeeping `json:"housekeeping"`
}

// Housekeeping is the daily check-forget-prune pass.
type Housekeeping struct {
	Enabled bool `json:"enabled"`
	// At is a time of day, "HH:MM".
	At string `json:"at"`
	// TZ is an IANA zone name; empty means the daemon's local zone. This is a
	// scheduling zone, not a display one -- 04:30 has to stay 04:30 across a
	// daylight-saving change, which is why it is stored rather than rendered.
	TZ string `json:"tz"`

	Check      bool `json:"check"`
	Forget     bool `json:"forget"`
	Prune      bool `json:"prune"`
	EmptyTrash bool `json:"empty_trash"`
}

// Retention mirrors repo.Policy with JSON tags and a readable duration. See
// repo.Policy for why it is a mirror rather than the thing itself.
type Retention struct {
	KeepLast     int      `json:"keep_last"`
	KeepHourly   int      `json:"keep_hourly"`
	KeepDaily    int      `json:"keep_daily"`
	KeepWeekly   int      `json:"keep_weekly"`
	KeepMonthly  int      `json:"keep_monthly"`
	KeepYearly   int      `json:"keep_yearly"`
	KeepWithin   string   `json:"keep_within"`
	KeepTags     []string `json:"keep_tags"`
	MinSnapshots int      `json:"min_snapshots"`
}

// Exclusions is what a backup leaves out, and what it reads under quiesce.
type Exclusions struct {
	// UseDefaults applies exclude.DefaultAMPExclusions.
	UseDefaults bool `json:"use_defaults"`
	// HonourAMP reads AMP's own .backupExclude files.
	HonourAMP bool `json:"honour_amp"`
	// Patterns are the operator's own rules, applied after the built-ins so
	// that one of them can negate a built-in.
	Patterns []string `json:"patterns"`
	// Hot are the paths read between save-off and save-on. Empty means
	// exclude.DefaultHotPatterns.
	Hot []string `json:"hot"`
}

// Defaults reproduces what the systemd units it replaces did, so that
// migrating changes nothing an operator would notice.
func Defaults() File {
	return File{
		Version: Version,
		Schedule: Schedule{
			Enabled:      true,
			Every:        "1h",
			Jitter:       "5m",
			Quiesce:      true,
			StartupGrace: "2m",
			Housekeeping: Housekeeping{
				Enabled: true, At: "04:30",
				Check: true, Forget: true, Prune: true, EmptyTrash: true,
			},
		},
		Retention:  FromPolicy(repo.DefaultPolicy()),
		Exclusions: Exclusions{UseDefaults: true, HonourAMP: true},
		UpdatedBy:  "install",
	}
}

// FromPolicy converts a repository policy into its serialisable mirror.
func FromPolicy(p repo.Policy) Retention {
	return Retention{
		KeepLast:     p.KeepLast,
		KeepHourly:   p.KeepHourly,
		KeepDaily:    p.KeepDaily,
		KeepWeekly:   p.KeepWeekly,
		KeepMonthly:  p.KeepMonthly,
		KeepYearly:   p.KeepYearly,
		KeepWithin:   p.KeepWithin.String(),
		KeepTags:     slices.Clone(p.KeepTags),
		MinSnapshots: p.MinSnapshots,
	}
}

// Policy converts back.
func (r Retention) Policy() (repo.Policy, error) {
	within, err := parseDuration("retention.keep_within", r.KeepWithin)
	if err != nil {
		return repo.Policy{}, err
	}
	p := repo.Policy{
		KeepLast:     r.KeepLast,
		KeepHourly:   r.KeepHourly,
		KeepDaily:    r.KeepDaily,
		KeepWeekly:   r.KeepWeekly,
		KeepMonthly:  r.KeepMonthly,
		KeepYearly:   r.KeepYearly,
		KeepWithin:   within,
		KeepTags:     slices.Clone(r.KeepTags),
		MinSnapshots: r.MinSnapshots,
	}
	return p, p.Validate()
}

// Interval returns how often a backup should run.
func (s Schedule) Interval() (time.Duration, error) {
	return parseDuration("schedule.every", s.Every)
}

// JitterWindow returns the randomisation window, which may be zero.
func (s Schedule) JitterWindow() (time.Duration, error) {
	if strings.TrimSpace(s.Jitter) == "" {
		return 0, nil
	}
	return parseDuration("schedule.jitter", s.Jitter)
}

// Grace returns the delay before the first fire after a restart.
func (s Schedule) Grace() (time.Duration, error) {
	if strings.TrimSpace(s.StartupGrace) == "" {
		return 0, nil
	}
	return parseDuration("schedule.startup_grace", s.StartupGrace)
}

// Location resolves the zone housekeeping is scheduled in.
func (h Housekeeping) Location() (*time.Location, error) {
	if strings.TrimSpace(h.TZ) == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(h.TZ)
	if err != nil {
		return nil, fmt.Errorf("settings: unknown time zone %q: %w", h.TZ, err)
	}
	return loc, nil
}

// TimeOfDay returns the hour and minute housekeeping runs at.
func (h Housekeeping) TimeOfDay() (hour, minute int, err error) {
	at := strings.TrimSpace(h.At)
	if _, err := fmt.Sscanf(at, "%d:%d", &hour, &minute); err != nil {
		return 0, 0, fmt.Errorf("settings: housekeeping.at %q is not HH:MM", h.At)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("settings: housekeeping.at %q is not a time of day", h.At)
	}
	return hour, minute, nil
}

// DoesAnything reports whether the housekeeping pass would do anything at all.
// A pass with every step switched off is a timer that wakes up to do nothing,
// which is worth refusing rather than running.
func (h Housekeeping) DoesAnything() bool {
	return h.Check || h.Forget || h.Prune || h.EmptyTrash
}

// Validate checks everything that could only otherwise fail at fire time --
// which, for a schedule, means in the middle of the night with nobody looking.
func (f File) Validate() error {
	if f.Version != Version {
		return fmt.Errorf("settings: version %d, expected %d", f.Version, Version)
	}
	if f.Instance.Name == "" {
		return errors.New("settings: no instance name")
	}
	if f.Instance.Root == "" {
		return errors.New("settings: no instance root")
	}
	if !filepath.IsAbs(f.Instance.Root) {
		return fmt.Errorf("settings: instance root %q is not absolute", f.Instance.Root)
	}

	every, err := f.Schedule.Interval()
	if err != nil {
		return err
	}
	if every < time.Minute {
		return fmt.Errorf("settings: schedule.every is %s; a backup every minute would never finish before the next one starts", every)
	}
	jitter, err := f.Schedule.JitterWindow()
	if err != nil {
		return err
	}
	if jitter >= every {
		return fmt.Errorf("settings: schedule.jitter (%s) must be shorter than schedule.every (%s)", jitter, every)
	}
	if _, err := f.Schedule.Grace(); err != nil {
		return err
	}

	if f.Schedule.Housekeeping.Enabled {
		if _, _, err := f.Schedule.Housekeeping.TimeOfDay(); err != nil {
			return err
		}
		if _, err := f.Schedule.Housekeeping.Location(); err != nil {
			return err
		}
		if !f.Schedule.Housekeeping.DoesAnything() {
			return errors.New("settings: housekeeping is enabled but every step is switched off")
		}
	}

	if _, err := f.Retention.Policy(); err != nil {
		return err
	}
	if _, err := exclude.Compile(f.Exclusions.Patterns); err != nil {
		return err
	}
	if _, err := exclude.Compile(f.Exclusions.Hot); err != nil {
		return err
	}
	return nil
}

// Clone returns a deep copy, so that a caller holding one cannot reach into
// the store's own state through a shared slice.
func (f File) Clone() File {
	f.Retention.KeepTags = slices.Clone(f.Retention.KeepTags)
	f.Exclusions.Patterns = slices.Clone(f.Exclusions.Patterns)
	f.Exclusions.Hot = slices.Clone(f.Exclusions.Hot)
	return f
}

func parseDuration(field, value string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("settings: %s %q is not a duration: %w", field, value, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("settings: %s %q is negative", field, value)
	}
	return d, nil
}

// Store owns the file on disk and notifies whoever is watching it.
type Store struct {
	path string

	mu          sync.RWMutex
	cur         File
	subscribers []chan File
}

// Open reads the settings file, creating it from seed if it does not exist.
//
// The seed carries the parts that come from the command line -- which instance,
// where it lives -- so that an operator configures once at install time and
// everything after that happens in the browser.
func Open(path string, seed File) (*Store, error) {
	s := &Store{path: path}

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var f File
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("settings: %s is not readable as settings: %w", path, err)
		}
		// The command line still decides what is being backed up: the operator
		// may have moved the instance, and a stale root in a file nobody
		// remembers editing is a backup of the wrong directory.
		if seed.Instance.Name != "" {
			f.Instance = seed.Instance
		}
		if err := f.Validate(); err != nil {
			return nil, err
		}
		s.cur = f
		return s, nil

	case errors.Is(err, os.ErrNotExist):
		if err := seed.Validate(); err != nil {
			return nil, err
		}
		seed.UpdatedAt = time.Now().UTC()
		if err := s.write(seed); err != nil {
			return nil, err
		}
		s.cur = seed
		return s, nil

	default:
		return nil, fmt.Errorf("settings: reading %s: %w", path, err)
	}
}

// NewMemory returns a store that is not backed by a file.
//
// The CLI uses it: its flags describe one run, there is nothing to persist,
// and it still has to assemble that run exactly the way the daemon does. An
// Update on such a store validates and publishes but writes nowhere.
func NewMemory(f File) (*Store, error) {
	f.Version = Version
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return &Store{cur: f}, nil
}

// Path returns the file the store is backed by, or "" for an in-memory one.
func (s *Store) Path() string { return s.path }

// Get returns the current settings.
func (s *Store) Get() File {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.Clone()
}

// Update applies fn to a copy, validates the result, writes it atomically and
// only then publishes it. A rejected change leaves both the file and the
// running daemon exactly as they were.
func (s *Store) Update(by string, fn func(*File) error) (File, error) {
	s.mu.Lock()
	next := s.cur.Clone()
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return File{}, err
	}
	next.Version = Version
	// The instance is not editable through this path; see Instance.
	next.Instance = s.cur.Instance
	next.UpdatedAt = time.Now().UTC()
	next.UpdatedBy = by

	if err := next.Validate(); err != nil {
		s.mu.Unlock()
		return File{}, err
	}
	if err := s.write(next); err != nil {
		s.mu.Unlock()
		return File{}, err
	}
	s.cur = next
	subs := slices.Clone(s.subscribers)
	s.mu.Unlock()

	for _, ch := range subs {
		// Non-blocking: a subscriber that has not drained its last update does
		// not get to stall the settings editor. It will read the current value
		// when it wakes, and the current value is all it ever wanted.
		select {
		case ch <- next.Clone():
		default:
		}
	}
	return next.Clone(), nil
}

// Subscribe returns a channel that receives every accepted change, and a
// function that stops it.
func (s *Store) Subscribe() (<-chan File, func()) {
	ch := make(chan File, 1)
	s.mu.Lock()
	s.subscribers = append(s.subscribers, ch)
	s.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.mu.Lock()
			s.subscribers = slices.DeleteFunc(s.subscribers, func(c chan File) bool { return c == ch })
			s.mu.Unlock()
			close(ch)
		})
	}
}

func (s *Store) write(f File) error {
	if s.path == "" {
		return nil
	}
	return atomicfile.WriteJSON(s.path, f, 0o640)
}
