package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/spf13/cobra"
)

func newForgetCommand() *cobra.Command {
	var (
		policy   repo.Policy
		instance string
		apply    bool
		within   string
	)
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Remove snapshots from the live set according to a retention policy",
		Long: "Applies a grandfather-father-son policy and moves the snapshots it does not\n" +
			"want into the repository's trash.\n\n" +
			"This never deletes file contents. Objects are freed only by `prune`, which\n" +
			"is a separate step, so a policy that turns out to be wrong costs nothing but\n" +
			"an `unforget`.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			if within != "" {
				d, err := time.ParseDuration(within)
				if err != nil {
					return fmt.Errorf("--keep-within: %w", err)
				}
				policy.KeepWithin = d
			}

			all, err := r.ListSnapshots()
			if err != nil {
				return err
			}
			var candidates []repo.Manifest
			for _, m := range all {
				if instance == "" || m.Instance == instance {
					candidates = append(candidates, m)
				}
			}
			if len(candidates) == 0 {
				fmt.Println("No snapshots to consider.")
				return nil
			}

			decisions, err := policy.Apply(candidates, time.Now().UTC())
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ACTION\tID\tTAKEN\tWHY")
			var doomed []string
			for _, d := range decisions {
				action, why := "keep", strings.Join(d.Reasons, "; ")
				if !d.Keep {
					action, why = "forget", "no rule keeps it"
					doomed = append(doomed, d.Manifest.ID)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
					action, d.Manifest.ID, d.Manifest.StartedAt.Format("2006-01-02 15:04"), why)
			}
			w.Flush()

			if len(doomed) == 0 {
				fmt.Println("\nNothing to forget.")
				return nil
			}
			if !apply {
				fmt.Printf("\n%d snapshot(s) would be forgotten. Re-run with --apply to do it.\n",
					len(doomed))
				return nil
			}
			if err := r.Forget(doomed); err != nil {
				return err
			}
			fmt.Printf("\nForgot %d snapshot(s). Their data is still in the repository;\n"+
				"run `amp-bb prune --force` to reclaim the space, or `amp-bb unforget <id>`\n"+
				"to put one back.\n", len(doomed))
			return nil
		},
	}

	def := repo.DefaultPolicy()
	f := cmd.Flags()
	f.IntVar(&policy.KeepLast, "keep-last", def.KeepLast, "always keep the N most recent snapshots")
	f.IntVar(&policy.KeepHourly, "keep-hourly", def.KeepHourly, "keep the newest snapshot of each of the last N hours")
	f.IntVar(&policy.KeepDaily, "keep-daily", def.KeepDaily, "…of each of the last N days")
	f.IntVar(&policy.KeepWeekly, "keep-weekly", def.KeepWeekly, "…of each of the last N weeks")
	f.IntVar(&policy.KeepMonthly, "keep-monthly", def.KeepMonthly, "…of each of the last N months")
	f.IntVar(&policy.KeepYearly, "keep-yearly", def.KeepYearly, "…of each of the last N years")
	f.StringVar(&within, "keep-within", def.KeepWithin.String(), "keep everything younger than this duration")
	f.StringArrayVar(&policy.KeepTags, "keep-tag", def.KeepTags, "never forget snapshots with this tag (repeatable)")
	f.IntVar(&policy.MinSnapshots, "min-snapshots", def.MinSnapshots, "hard floor that overrides every other rule")
	f.StringVar(&instance, "instance", "", "only consider snapshots of this instance")
	f.BoolVar(&apply, "apply", false, "actually forget; without this the command only reports")
	return cmd
}

func newUnforgetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unforget <snapshot>",
		Short: "Put a forgotten snapshot back into the live set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			if err := r.Unforget(args[0]); err != nil {
				return err
			}
			fmt.Printf("Snapshot %s is live again.\n", args[0])
			return nil
		},
	}
}

func newPruneCommand() *cobra.Command {
	var (
		force      bool
		grace      time.Duration
		emptyTrash bool
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete file contents that no snapshot references any more",
		Long: "This is the only command that deletes data. It reports by default and\n" +
			"requires --force to actually remove anything.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			if force && !emptyTrash {
				// Silently keeping the trash would make --force look broken:
				// the user asks for space back and gets none.
				fmt.Println("Note: forgotten snapshots are being kept. Pass --empty-trash to")
				fmt.Println("      release their data as well.")
			}

			rep, err := r.Prune(ctx, repo.PruneOptions{
				DryRun:      !force,
				GracePeriod: grace,
				EmptyTrash:  emptyTrash,
			})
			if err != nil {
				return err
			}

			verb := "Would delete"
			if force {
				verb = "Deleted"
			}
			fmt.Printf("Snapshots     %d live, %d in the trash\n", rep.LiveSnapshots, rep.TrashedSnapshots)
			fmt.Printf("Objects       %d total, %d still referenced\n", rep.TotalObjects, rep.ReferencedObjects)
			fmt.Printf("%-13s %d objects, %s\n", verb, rep.DeletedObjects, humanBytes(rep.FreedBytes))
			if rep.SparedRecent > 0 {
				fmt.Printf("Spared        %d unreferenced object(s) younger than %s\n",
					rep.SparedRecent, grace)
			}
			if !force && rep.DeletedObjects > 0 {
				fmt.Println("\nRe-run with --force to actually delete. Take a `check --read-data` first")
				fmt.Println("if this repository has never been verified.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "actually delete; without this the command only reports")
	cmd.Flags().DurationVar(&grace, "grace", repo.DefaultGracePeriod,
		"leave unreferenced objects younger than this alone")
	cmd.Flags().BoolVar(&emptyTrash, "empty-trash", false,
		"also discard forgotten snapshots, releasing their data")
	return cmd
}

func newCheckCommand() *cobra.Command {
	var readData bool
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Verify that every snapshot is complete and readable",
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := openRepo()
			if err != nil {
				return err
			}
			ctx, stop := signalContext()
			defer stop()

			rep, err := r.Check(ctx, repo.CheckOptions{ReadData: readData})
			if err != nil {
				return err
			}
			fmt.Printf("Snapshots   %d checked\n", rep.Snapshots)
			fmt.Printf("References  %d distinct objects\n", rep.Objects)
			if readData {
				fmt.Printf("Re-hashed   %d objects\n", rep.Rehashed)
			}
			if len(rep.Problems) == 0 {
				fmt.Println("No problems found.")
				return nil
			}
			for _, p := range rep.Problems {
				fmt.Println(" ", p)
			}
			return errors.New("repository check failed")
		},
	}
	cmd.Flags().BoolVar(&readData, "read-data", false,
		"read and re-hash every referenced object, not just check that it exists")
	return cmd
}
