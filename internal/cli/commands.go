package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/backup"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/exclude"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/format"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/quiesce"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/restore"
	"github.com/spf13/cobra"
)

// signalContext cancels on SIGINT/SIGTERM so a run can unwind cleanly — which
// for a live backup means releasing the quiesce.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func openRepo() (*repo.Repository, error) {
	path, err := requireRepoPath()
	if err != nil {
		return nil, err
	}
	return repo.Open(path)
}

func newInitCommand() *cobra.Command {
	var level int
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a new backup repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := requireRepoPath()
			if err != nil {
				return err
			}
			r, err := repo.Init(path, level)
			if err != nil {
				return err
			}
			fmt.Printf("Initialised repository at %s\n", r.Root())
			fmt.Printf("  hash        %s\n", r.Config().HashAlgo)
			fmt.Printf("  compression %s level %d\n", r.Config().CompressionAlgo, r.Config().CompressionLevel)
			return nil
		},
	}
	cmd.Flags().IntVar(&level, "compression", 3, "zstd compression level (1 fastest, 19 smallest)")
	return cmd
}

func newBackupCommand() *cobra.Command {
	var (
		instance    string
		root        string
		excludes    []string
		hot         []string
		tags        []string
		paranoid    bool
		ampExclude  bool
		useDefaults bool
		reserveGiB  float64
		quiet       bool
		doQuiesce   bool
		confirmPat  string
		ampCfg      ampFlags
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Take a snapshot of an instance directory",
		Long: "Reads an instance directory and stores everything that changed since\n" +
			"the previous snapshot. Without --quiesce-* options the application is\n" +
			"assumed to be stopped or idle; this is the safe mode to start with.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if instance == "" {
				return errors.New("--instance is required")
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			// A panel connection serves two purposes: finding the instance
			// directory without guessing a path, and quiescing the application
			// while its live files are read.
			var ampClient *amp.Client
			if ampCfg.configured() {
				if ampClient, err = ampCfg.client(); err != nil {
					return err
				}
			}
			if root == "" {
				if ampClient == nil {
					return errors.New("pass --root, or --amp-url so the instance directory can be looked up")
				}
				if root, err = resolveRoot(ctx, ampClient, instance); err != nil {
					return err
				}
				if !quiet {
					fmt.Printf("Instance %s lives at %s\n", instance, root)
				}
			}

			quiescer := backup.Quiescer(backup.NoQuiesce{Reason: "not requested"})
			if doQuiesce {
				if ampClient == nil {
					return errors.New("--quiesce needs --amp-url, --amp-user and a password")
				}
				pattern := quiesce.DefaultConfirmPattern
				if confirmPat != "" {
					if pattern, err = regexp.Compile(confirmPat); err != nil {
						return fmt.Errorf("--confirm-pattern: %w", err)
					}
				}
				quiescer = quiesce.NewConsole(ampClient, quiesce.Config{
					ConfirmPattern: pattern,
					SkipIfStopped:  true,
					Log: func(format string, args ...any) {
						fmt.Fprintf(os.Stderr, "quiesce: "+format+"\n", args...)
					},
				})
			}

			// Built-in rules come first so a user pattern can negate them.
			var patterns []string
			if useDefaults {
				patterns = append(patterns, exclude.DefaultAMPExclusions...)
			}
			patterns = append(patterns, excludes...)
			if ampExclude {
				found, err := exclude.CollectAMPExcludes(root)
				if err != nil {
					return fmt.Errorf("reading AMP exclusions: %w", err)
				}
				if len(found) > 0 && !quiet {
					fmt.Printf("Honouring %d rule(s) from AMP's own %s files\n",
						len(found), exclude.AMPExcludeFile)
				}
				patterns = append(patterns, found...)
			}
			excludeSet, err := exclude.Compile(patterns)
			if err != nil {
				return err
			}
			hotPatterns := hot
			if len(hotPatterns) == 0 {
				hotPatterns = exclude.DefaultHotPatterns
			}
			hotSet, err := exclude.Compile(hotPatterns)
			if err != nil {
				return err
			}

			started := time.Now()
			m, err := backup.Run(ctx, r, backup.Options{
				Instance:     instance,
				Root:         root,
				Exclude:      excludeSet,
				Hot:          hotSet,
				Tags:         tags,
				Quiescer:     quiescer,
				Paranoid:     paranoid,
				SkipAbs:      []string{r.Root()},
				ReserveBytes: reserveBytes(reserveGiB),
				ToolVersion:  Version,
				Progress:     progressPrinter(quiet),
			})
			if err != nil {
				return err
			}
			printBackupSummary(m, time.Since(started))
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "AMP instance name (groups snapshots)")
	cmd.Flags().StringVar(&root, "root", "",
		"instance directory to back up; looked up via the AMP API when omitted")
	cmd.Flags().StringArrayVar(&excludes, "exclude", nil, "path pattern to skip (repeatable)")
	cmd.Flags().StringArrayVar(&hot, "hot", nil,
		"pattern for paths that must be read under quiesce (repeatable; defaults to Minecraft world paths)")
	cmd.Flags().StringArrayVar(&tags, "tag", nil, "tag to attach to the snapshot (repeatable)")
	cmd.Flags().BoolVar(&paranoid, "paranoid", false,
		"re-read every file instead of trusting size and timestamp")
	cmd.Flags().BoolVar(&ampExclude, "amp-exclusions", true,
		"honour AMP's own .backupExclude files")
	cmd.Flags().BoolVar(&useDefaults, "default-exclusions", true,
		"apply the built-in exclusions (AMP's own Backups directory, logs, locks, rendered maps)")
	cmd.Flags().Float64Var(&reserveGiB, "reserve", 2,
		"gibibytes of free space the run must leave untouched; 0 disables the check")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output")
	cmd.Flags().BoolVar(&doQuiesce, "quiesce", false,
		"hold the application still while its live files are read (save-off / save-all flush / save-on)")
	cmd.Flags().StringVar(&confirmPat, "confirm-pattern", "",
		"regexp for the console line that confirms the flush (defaults to Minecraft's)")
	ampCfg.register(cmd)
	return cmd
}

func progressPrinter(quiet bool) func(string, int, int) {
	if quiet {
		return nil
	}
	last := time.Now()
	return func(stage string, done, total int) {
		if total == 0 {
			return
		}
		if done != total && time.Since(last) < 500*time.Millisecond {
			return
		}
		last = time.Now()
		fmt.Printf("\r  %-5s %d/%d", stage, done, total)
		if done == total {
			fmt.Println()
		}
	}
}

func printBackupSummary(m *repo.Manifest, wall time.Duration) {
	fmt.Printf("\nSnapshot %s  (%s)\n", m.ID, m.State)
	if m.Parent != "" {
		fmt.Printf("  parent          %s\n", m.Parent)
	}
	fmt.Printf("  contents        %d files, %d dirs, %d symlinks, %s\n",
		m.Stats.Files, m.Stats.Dirs, m.Stats.Symlinks, format.Bytes(m.Stats.TotalBytes))
	fmt.Printf("  unchanged       %d files skipped without reading\n", m.Stats.UnchangedFiles)
	fmt.Printf("  deduplicated    %d objects already present\n", m.Stats.ReusedObjects)
	fmt.Printf("  written         %d new objects, %s\n",
		m.Stats.NewObjects, format.Bytes(m.Stats.NewBytes))
	if m.Stats.RereadFiles > 0 {
		// Visible, but not alarming: these settled, or they would have warned.
		fmt.Printf("  re-read         %d file(s) that changed while being read\n",
			m.Stats.RereadFiles)
	}
	fmt.Printf("  quiesce window  %d ms\n", m.QuiesceMillis)
	fmt.Printf("  wall time       %s\n", wall.Round(time.Millisecond))

	if m.Stats.TotalBytes > 0 && m.Stats.NewBytes >= 0 {
		saved := 100 * (1 - float64(m.Stats.NewBytes)/float64(m.Stats.TotalBytes))
		fmt.Printf("  this snapshot cost %s instead of %s (%.2f%% saved)\n",
			format.Bytes(m.Stats.NewBytes), format.Bytes(m.Stats.TotalBytes), saved)
	}
	for _, w := range m.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}

func newSnapshotsCommand() *cobra.Command {
	var instance string
	cmd := &cobra.Command{
		Use:     "snapshots",
		Aliases: []string{"list"},
		Short:   "List snapshots in the repository",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			all, err := r.ListSnapshots()
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tINSTANCE\tTAKEN\tSTATE\tFILES\tSIZE\tNEW\tQUIESCE")
			var shown int
			for _, m := range all {
				if instance != "" && m.Instance != instance {
					continue
				}
				shown++
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%d ms\n",
					m.ID, m.Instance, m.StartedAt.Format("2006-01-02 15:04"), m.State,
					m.Stats.Files, format.Bytes(m.Stats.TotalBytes),
					format.Bytes(m.Stats.NewBytes), m.QuiesceMillis)
			}
			w.Flush()
			if shown == 0 {
				fmt.Println("(no snapshots)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "", "only show snapshots of this instance")
	return cmd
}

func newRestoreCommand() *cobra.Command {
	var (
		target    string
		include   []string
		dryRun    bool
		overwrite bool
		times     bool
	)
	cmd := &cobra.Command{
		Use:   "restore <snapshot>",
		Short: "Write a snapshot back out to a directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if target == "" {
				return errors.New("--target is required")
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			var includeSet *exclude.Set
			if len(include) > 0 {
				if includeSet, err = exclude.Compile(include); err != nil {
					return err
				}
			}

			rep, err := restore.Run(ctx, r, restore.Options{
				Snapshot: args[0], Target: target, Include: includeSet,
				DryRun: dryRun, Overwrite: overwrite, RestoreTimes: times,
			})
			if err != nil {
				return err
			}
			verb := "Restored"
			if rep.DryRun {
				verb = "Would restore"
			}
			fmt.Printf("%s %d files, %d dirs, %d symlinks (%s) to %s\n",
				verb, rep.Files, rep.Dirs, rep.Symlinks, format.Bytes(rep.Bytes), target)
			if !rep.DryRun {
				fmt.Printf("Every restored file was re-hashed and matched the snapshot (%d/%d).\n",
					rep.Verified, rep.Files)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "directory to restore into")
	cmd.Flags().StringArrayVar(&include, "include", nil, "only restore matching paths (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen and stop")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "allow writing into a non-empty target")
	cmd.Flags().BoolVar(&times, "times", true, "restore modification times")
	return cmd
}

func newVerifyCommand() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "verify <snapshot>",
		Short: "Compare a directory against a snapshot, file by file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if target == "" {
				return errors.New("--target is required")
			}
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			diffs, err := restore.Verify(ctx, r, args[0], target)
			if err != nil {
				return err
			}
			if len(diffs) == 0 {
				fmt.Printf("%s matches snapshot %s exactly.\n", target, args[0])
				return nil
			}
			for _, d := range diffs {
				fmt.Println(" ", d)
			}
			return fmt.Errorf("%d difference(s) between %s and snapshot %s", len(diffs), target, args[0])
		},
	}
	cmd.Flags().StringVar(&target, "target", "", "directory to compare against")
	return cmd
}

func newStatsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stats",
		Short: "Report repository size and deduplication ratio",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			var objects int64
			var stored int64
			if err := r.Objects().List(ctx, func(info repo.ObjectInfo) error {
				objects++
				stored += info.StoredSize
				return nil
			}); err != nil {
				return err
			}

			all, err := r.ListSnapshots()
			if err != nil {
				return err
			}
			var logical int64
			for _, m := range all {
				logical += m.Stats.TotalBytes
			}

			fmt.Printf("Repository   %s\n", r.Root())
			fmt.Printf("Snapshots    %d\n", len(all))
			fmt.Printf("Objects      %d\n", objects)
			fmt.Printf("On disk      %s\n", format.Bytes(stored))
			fmt.Printf("Logical      %s  (what the snapshots describe in total)\n", format.Bytes(logical))
			if stored > 0 && logical > 0 {
				fmt.Printf("Ratio        %.1fx  (%s saved)\n",
					float64(logical)/float64(stored), format.Bytes(logical-stored))
			}
			return nil
		},
	}
}

func newLsCommand() *cobra.Command {
	var prefix string
	cmd := &cobra.Command{
		Use:   "ls <snapshot>",
		Short: "List the paths a snapshot contains",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			ir, closeIdx, err := r.OpenIndex(args[0])
			if err != nil {
				return err
			}
			defer closeIdx()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for {
				e, err := ir.Next()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				if prefix != "" && !strings.HasPrefix(e.Path, prefix) {
					continue
				}
				switch e.Type {
				case repo.TypeDir:
					fmt.Fprintf(w, "d\t%o\t-\t%s\n", e.Mode.Perm(), e.Path)
				case repo.TypeSymlink:
					fmt.Fprintf(w, "l\t-\t-\t%s -> %s\n", e.Path, e.Target)
				default:
					fmt.Fprintf(w, "f\t%o\t%s\t%s\n", e.Mode.Perm(), format.Bytes(e.Size), e.Path)
				}
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&prefix, "prefix", "", "only list paths under this prefix")
	return cmd
}

// reserveBytes converts the --reserve flag. A zero from the user means "do not
// check", which backup.Options expresses as a negative value so that it is
// distinguishable from an unset field.
func reserveBytes(gib float64) int64 {
	if gib <= 0 {
		return -1
	}
	return int64(gib * float64(1<<30))
}
