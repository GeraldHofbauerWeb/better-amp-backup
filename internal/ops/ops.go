// Package ops turns settings into the calls the backup, restore and repository
// packages expose.
//
// It is the single place that decides what a backup of this instance means:
// which rules apply in which order, which paths are read under quiesce, how
// much disk has to be left over. The CLI and the daemon both go through it, so
// a backup started from a browser and one typed at a terminal cannot drift
// apart -- and drift is exactly what would happen otherwise, quietly, over a
// month, until one of them produced a snapshot the other could not explain.
package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/backup"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/format"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/quiesce"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/restore"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
)

// Progress is how a long operation reports. jobs.Handle satisfies it; the CLI
// passes an adapter that prints.
type Progress interface {
	Stage(stage string, done, total int)
	Logf(level, format string, args ...any)
}

// Discard is a Progress that reports nowhere.
type Discard struct{}

func (Discard) Stage(string, int, int)      {}
func (Discard) Logf(string, string, ...any) {}

// Runner performs the operations. Everything it needs to know about what is
// being backed up comes from Settings.
type Runner struct {
	Repo     *repo.Repository
	Settings *settings.Store
	// Instance returns a client for the instance, built from the service
	// account. Backups and restores use this and never a user's session: an
	// unattended run at three in the morning has no user.
	Instance func(ctx context.Context) (*amp.Client, error)
	// ToolVersion is stamped into every snapshot.
	ToolVersion string
	// ScratchDir is where a look-before-you-leap restore materialises.
	ScratchDir string
	// Now is a test seam.
	Now func() time.Time
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// BackupRequest is one backup.
type BackupRequest struct {
	Tags []string
	// Quiesce overrides the configured setting when non-nil. A pre-restore
	// snapshot sets it to false: the application is already stopped, and
	// sending save-off to a stopped server accomplishes nothing but noise.
	Quiesce *bool
	// Paranoid re-reads and re-hashes everything instead of trusting the stat
	// difference.
	Paranoid bool
	// ReserveBytes is the free space the run must leave untouched. Zero takes
	// the built-in default; negative disables the check, which is the CLI's
	// documented meaning for --reserve 0.
	ReserveBytes int64
	// ConfirmPattern overrides the console line that confirms a flush, for a
	// server that is not Minecraft or has been reworded.
	ConfirmPattern *regexp.Regexp
}

// Backup takes a snapshot.
func (r *Runner) Backup(ctx context.Context, p Progress, req BackupRequest) (*repo.Manifest, error) {
	cfg := r.Settings.Get()

	excludeSet, err := r.excludeSet(cfg, p)
	if err != nil {
		return nil, err
	}
	hotSet, err := hotSet(cfg)
	if err != nil {
		return nil, err
	}

	wantQuiesce := cfg.Schedule.Quiesce
	if req.Quiesce != nil {
		wantQuiesce = *req.Quiesce
	}
	quiescer := backup.Quiescer(backup.NoQuiesce{Reason: "not requested"})
	if wantQuiesce {
		if r.Instance == nil {
			return nil, errors.New("ops: quiescing needs an AMP connection, and none is configured")
		}
		client, err := r.Instance(ctx)
		if err != nil {
			return nil, err
		}
		pattern := req.ConfirmPattern
		if pattern == nil {
			pattern = quiesce.DefaultConfirmPattern
		}
		quiescer = quiesce.NewConsole(client, quiesce.Config{
			ConfirmPattern: pattern,
			SkipIfStopped:  true,
			Log: func(f string, args ...any) {
				// Until now this only reached the journal. It is the most
				// interesting thing the tool does to a live server, and it
				// belongs where whoever pressed the button can read it.
				p.Logf("info", "quiesce: "+f, args...)
			},
		})
	}

	return backup.Run(ctx, r.Repo, backup.Options{
		Instance:   cfg.Instance.Name,
		InstanceID: cfg.Instance.ID,
		Root:       cfg.Instance.Root,
		Exclude:    excludeSet,
		Hot:        hotSet,
		Tags:       req.Tags,
		Quiescer:   quiescer,
		Paranoid:   req.Paranoid,
		// Never descend into our own repository, whatever else is configured.
		SkipAbs:      []string{r.Repo.Root()},
		ReserveBytes: req.ReserveBytes,
		ToolVersion:  r.ToolVersion,
		Progress:     p.Stage,
	})
}

// excludeSet assembles the rules in the order that makes a user pattern able
// to negate a built-in one. The order is load-bearing, not incidental.
func (r *Runner) excludeSet(cfg settings.File, p Progress) (*exclude.Set, error) {
	var patterns []string
	if cfg.Exclusions.UseDefaults {
		patterns = append(patterns, exclude.DefaultAMPExclusions...)
	}
	patterns = append(patterns, cfg.Exclusions.Patterns...)
	if cfg.Exclusions.HonourAMP {
		found, err := exclude.CollectAMPExcludes(cfg.Instance.Root)
		if err != nil {
			return nil, fmt.Errorf("ops: reading AMP exclusions: %w", err)
		}
		if len(found) > 0 {
			p.Logf("info", "honouring %d rule(s) from AMP's own %s files",
				len(found), exclude.AMPExcludeFile)
		}
		patterns = append(patterns, found...)
	}
	return exclude.Compile(patterns)
}

func hotSet(cfg settings.File) (*exclude.Set, error) {
	hot := cfg.Exclusions.Hot
	if len(hot) == 0 {
		hot = exclude.DefaultHotPatterns
	}
	return exclude.Compile(hot)
}

// TargetKind says where a restore writes.
//
// It is an enum of exactly two values because the daemon must never take a
// target path from a client: an absolute path arriving over HTTP is a write
// primitive handed to anyone who can reach the panel.
type TargetKind string

const (
	// TargetScratch writes into a fresh directory under ScratchDir. This is
	// the look-before-you-leap restore, and what the interface offers first.
	TargetScratch TargetKind = "scratch"
	// TargetInstance writes into the live instance directory.
	TargetInstance TargetKind = "instance"
)

// RestoreRequest is one restore.
type RestoreRequest struct {
	Snapshot string
	// Paths, when non-empty, restricts the restore to these exact paths and
	// everything beneath each of them. The browser sends the minimal cover: a
	// fully ticked directory is one path, not thirty thousand.
	Paths  []string
	Target TargetKind
	DryRun bool
	// PreSnapshot takes a snapshot of the current state first, tagged
	// pre-restore. Only meaningful for TargetInstance.
	PreSnapshot bool
}

// RestoreResult is what a restore produced.
type RestoreResult struct {
	PreSnapshot string          `json:"pre_snapshot,omitempty"`
	Report      *restore.Report `json:"report"`
	TargetPath  string          `json:"target_path"`
	// Planned is the dry run that preceded the real one, so the summary can
	// say what was expected as well as what happened.
	Planned *restore.Report `json:"planned,omitempty"`
}

// ErrInstanceRunning is returned when a restore into the live instance was
// asked for while the application is not stopped. The interface turns it into
// the banner with the stop button.
type ErrInstanceRunning struct{ State string }

func (e *ErrInstanceRunning) Error() string {
	return fmt.Sprintf("ops: the instance is %s; a restore into it needs it stopped", e.State)
}

// Restore puts a snapshot, or part of one, back.
func (r *Runner) Restore(ctx context.Context, p Progress, req RestoreRequest) (*RestoreResult, error) {
	cfg := r.Settings.Get()

	target, overwrite, err := r.target(cfg, req)
	if err != nil {
		return nil, err
	}
	out := &RestoreResult{TargetPath: target}

	if req.Target == TargetInstance {
		if err := r.requireStopped(ctx); err != nil {
			return nil, err
		}
		if req.PreSnapshot && !req.DryRun {
			p.Stage("pre-restore snapshot", 0, 0)
			no := false
			m, err := r.Backup(ctx, p, BackupRequest{
				Tags:    []string{"pre-restore"},
				Quiesce: &no, // the application is stopped; there is nothing to hold still
			})
			if err != nil {
				return nil, fmt.Errorf("ops: the pre-restore snapshot failed, so nothing was restored: %w", err)
			}
			out.PreSnapshot = m.ID
			p.Logf("info", "current state saved as %s before restoring", m.ID)
		}
	}

	// Always plan first. The counts go into the log, so that what happened can
	// be compared against what was supposed to happen.
	p.Stage("planning", 0, 0)
	planned, err := restore.Run(ctx, r.Repo, restore.Options{
		Snapshot: req.Snapshot, Target: target, IncludePaths: req.Paths,
		DryRun: true, Overwrite: overwrite, RestoreTimes: true,
	})
	if err != nil {
		return nil, err
	}
	out.Planned = planned
	for _, w := range planned.Warnings {
		p.Logf("warn", "%s", w)
	}
	p.Logf("info", "restoring %d file(s), %d dir(s), %s to %s",
		planned.Files, planned.Dirs, format.Bytes(planned.Bytes), target)

	if req.DryRun {
		out.Report = planned
		return out, nil
	}

	p.Stage("restoring", 0, planned.Files)
	rep, err := restore.Run(ctx, r.Repo, restore.Options{
		Snapshot: req.Snapshot, Target: target, IncludePaths: req.Paths,
		Overwrite: overwrite, RestoreTimes: true,
		Progress: func(done, total int) { p.Stage("restoring", done, total) },
	})
	if err != nil {
		return nil, err
	}
	out.Report = rep
	p.Logf("info", "%d file(s) re-hashed and matched the snapshot", rep.Verified)
	return out, nil
}

func (r *Runner) target(cfg settings.File, req RestoreRequest) (path string, overwrite bool, err error) {
	switch req.Target {
	case TargetInstance:
		return cfg.Instance.Root, true, nil
	case TargetScratch, "":
		if r.ScratchDir == "" {
			return "", false, errors.New("ops: no scratch directory configured")
		}
		name := fmt.Sprintf("%s-%s", req.Snapshot, r.now().UTC().Format("20060102T150405Z"))
		return filepath.Join(r.ScratchDir, name), false, nil
	default:
		return "", false, fmt.Errorf("ops: unknown restore target %q", req.Target)
	}
}

// requireStopped refuses unless the application is fully stopped.
//
// Not "not ready": starting, stopping, restarting, configuring and pending
// user input all mean a process may still hold those files open, and writing a
// world underneath a running server corrupts the very thing being restored.
func (r *Runner) requireStopped(ctx context.Context) error {
	if r.Instance == nil {
		return errors.New("ops: cannot check whether the instance is stopped: no AMP connection is configured")
	}
	client, err := r.Instance(ctx)
	if err != nil {
		return err
	}
	status, err := client.GetStatus(ctx)
	if err != nil {
		return err
	}
	if status.State != amp.StateStopped {
		return &ErrInstanceRunning{State: status.State.String()}
	}
	return nil
}

// HousekeepingRequest is the daily pass.
type HousekeepingRequest struct {
	Check      bool
	ReadData   bool
	Forget     bool
	Prune      bool
	EmptyTrash bool
	// DryRun reports what forget and prune would do without doing it.
	DryRun bool
}

// HousekeepingResult collects what each step produced.
type HousekeepingResult struct {
	Check     *repo.CheckReport `json:"check,omitempty"`
	Forgotten []repo.Decision   `json:"forgotten,omitempty"`
	Prune     *repo.PruneReport `json:"prune,omitempty"`
	DryRun    bool              `json:"dry_run"`
}

// Housekeeping runs check, forget and prune in the only order that is safe:
// prove the repository is intact before anything deletes from it.
func (r *Runner) Housekeeping(ctx context.Context, p Progress, req HousekeepingRequest) (*HousekeepingResult, error) {
	out := &HousekeepingResult{DryRun: req.DryRun}

	if req.Check {
		p.Stage("checking", 0, 0)
		report, err := r.Repo.Check(ctx, repo.CheckOptions{
			ReadData: req.ReadData,
			Progress: func(stage string, done, total int) { p.Stage("checking "+stage, done, total) },
		})
		if err != nil {
			return nil, err
		}
		out.Check = report
		if len(report.Problems) > 0 {
			for _, problem := range report.Problems {
				p.Logf("error", "%s", problem)
			}
			// Deleting from a repository that just failed its own integrity
			// check is how one bad snapshot becomes no snapshots.
			return out, fmt.Errorf("ops: the repository has %d problem(s); nothing was deleted", len(report.Problems))
		}
		p.Logf("info", "%d snapshot(s) and %d object(s) check out", report.Snapshots, report.Objects)
	}

	if req.Forget {
		p.Stage("applying retention", 0, 0)
		decisions, removed, err := r.applyRetention(ctx, req.DryRun)
		if err != nil {
			return nil, err
		}
		out.Forgotten = decisions
		verb := "forgot"
		if req.DryRun {
			verb = "would forget"
		}
		p.Logf("info", "%s %d snapshot(s)", verb, removed)
	}

	if req.Prune {
		p.Stage("pruning", 0, 0)
		report, err := r.Repo.Prune(ctx, repo.PruneOptions{
			DryRun:     req.DryRun,
			EmptyTrash: req.EmptyTrash,
			Progress:   func(stage string, done int) { p.Stage("pruning "+stage, done, 0) },
		})
		if err != nil {
			return nil, err
		}
		out.Prune = report
		p.Logf("info", "%d object(s) removed, %s reclaimed",
			report.DeletedObjects, format.Bytes(report.FreedBytes))
	}
	return out, nil
}

func (r *Runner) applyRetention(ctx context.Context, dryRun bool) ([]repo.Decision, int, error) {
	policy, err := r.Settings.Get().Retention.Policy()
	if err != nil {
		return nil, 0, err
	}
	snapshots, err := r.Repo.ListSnapshots()
	if err != nil {
		return nil, 0, err
	}
	decisions, err := policy.Apply(snapshots, r.now())
	if err != nil {
		return nil, 0, err
	}

	var doomed []string
	for _, d := range decisions {
		if !d.Keep {
			doomed = append(doomed, d.Manifest.ID)
		}
	}
	if dryRun || len(doomed) == 0 {
		return decisions, len(doomed), nil
	}
	if err := r.Repo.Forget(doomed); err != nil {
		return nil, 0, err
	}
	return decisions, len(doomed), nil
}

// RetentionPreview reports what the current policy would forget, changing
// nothing. It is what the settings editor shows before anything is applied.
func (r *Runner) RetentionPreview() ([]repo.Decision, error) {
	policy, err := r.Settings.Get().Retention.Policy()
	if err != nil {
		return nil, err
	}
	snapshots, err := r.Repo.ListSnapshots()
	if err != nil {
		return nil, err
	}
	return policy.Apply(snapshots, r.now())
}

// ExclusionPreview reports what a candidate set of rules would leave out of the
// next backup, measured against the newest snapshot's index.
//
// This is what turns the exclusions editor from "type a glob and hope" into a
// number. It reads an index rather than the live tree, so it costs nothing and
// cannot be fooled by a file that happens to be missing right now.
type ExclusionPreview struct {
	Snapshot string `json:"snapshot"`
	Entries  int    `json:"entries"`
	Excluded int    `json:"excluded"`
	Bytes    int64  `json:"bytes"`
	// Samples are a few of the paths that would be left out, so the number is
	// checkable rather than merely believable.
	Samples []string `json:"samples,omitempty"`
}

// PreviewExclusions measures a candidate rule set.
func (r *Runner) PreviewExclusions(patterns []string, useDefaults bool) (*ExclusionPreview, error) {
	var all []string
	if useDefaults {
		all = append(all, exclude.DefaultAMPExclusions...)
	}
	all = append(all, patterns...)
	set, err := exclude.Compile(all)
	if err != nil {
		return nil, err
	}

	latest, err := r.Repo.LatestFor(r.Settings.Get().Instance.Name)
	if err != nil {
		return nil, err
	}
	if latest == "" {
		return &ExclusionPreview{}, nil
	}

	ir, closeIdx, err := r.Repo.OpenIndex(latest)
	if err != nil {
		return nil, err
	}
	defer closeIdx()

	out := &ExclusionPreview{Snapshot: latest}
	for {
		e, err := ir.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
		out.Entries++
		if !set.Match(e.Path) {
			continue
		}
		out.Excluded++
		out.Bytes += e.Size
		if len(out.Samples) < 20 && e.Type == repo.TypeFile {
			out.Samples = append(out.Samples, e.Path)
		}
	}
	return out, nil
}

// DescribeSnapshot renders the one-line summary the interface and the CLI both
// want after a backup.
func DescribeSnapshot(m *repo.Manifest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d files, %s described, %s written",
		m.Stats.Files, format.Bytes(m.Stats.TotalBytes), format.Bytes(m.Stats.NewBytes))
	if m.Stats.RereadFiles > 0 {
		fmt.Fprintf(&b, ", %d re-read", m.Stats.RereadFiles)
	}
	if m.QuiesceMillis > 0 {
		fmt.Fprintf(&b, ", %dms quiesced", m.QuiesceMillis)
	}
	return b.String()
}
