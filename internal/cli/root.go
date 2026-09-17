// Package cli wires the commands together.
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

type globalFlags struct {
	repoPath string
}

var global globalFlags

// NewRootCommand builds the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "amp-bb",
		Short: "Incremental, deduplicating backups for CubeCoders AMP instances",
		Long: "amp-bb takes incremental snapshots of AMP instances.\n\n" +
			"Unlike AMP's built-in backup, which rewrites a full archive of the whole\n" +
			"instance on every run, amp-bb reads only what changed since the last\n" +
			"snapshot and stores it in a content-addressed repository.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
	}
	root.PersistentFlags().StringVar(&global.repoPath, "repo", envOr("AMPBB_REPO", ""),
		"path to the backup repository (env AMPBB_REPO)")

	root.AddCommand(
		newInitCommand(),
		newBackupCommand(),
		newSnapshotsCommand(),
		newRestoreCommand(),
		newVerifyCommand(),
		newStatsCommand(),
		newLsCommand(),
		newDoctorCommand(),
		newServeCommand(),
		newForgetCommand(),
		newUnforgetCommand(),
		newPruneCommand(),
		newCheckCommand(),
	)
	return root
}

// Execute runs the CLI and returns a process exit code.
func Execute() int {
	if err := NewRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func requireRepoPath() (string, error) {
	if global.repoPath == "" {
		return "", fmt.Errorf("no repository given: pass --repo or set AMPBB_REPO")
	}
	return global.repoPath, nil
}
