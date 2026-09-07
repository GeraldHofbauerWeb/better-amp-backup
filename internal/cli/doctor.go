package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/repo"
	"github.com/spf13/cobra"
)

// check is one line of the doctor report.
type check struct {
	name   string
	detail string
	level  level
}

type level int

const (
	levelOK level = iota
	levelWarn
	levelFail
)

func (l level) mark() string {
	switch l {
	case levelOK:
		return "ok  "
	case levelWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

// requiredMethods are the API calls this tool depends on. Checking them up
// front turns "the backup died at 03:00" into "doctor told you on day one".
var requiredMethods = [][2]string{
	{"Core", "Login"},
	{"Core", "GetStatus"},
	{"Core", "GetUpdates"},
	{"Core", "SendConsoleMessage"},
	{"ADSModule", "GetLocalInstances"},
}

func newDoctorCommand() *cobra.Command {
	var (
		ampCfg   ampFlags
		instance string
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the repository, the disk and the AMP panel are usable",
		Long: "Runs the checks that are cheap now and expensive at three in the morning:\n" +
			"is the repository readable, is there room to write into it, does the panel\n" +
			"answer, and does this AMP build still have the API methods this tool calls.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()

			var checks []check
			checks = append(checks, repositoryChecks()...)
			checks = append(checks, ampChecks(ctx, &ampCfg, instance)...)

			var failures int
			for _, c := range checks {
				fmt.Printf("[%s] %-26s %s\n", c.level.mark(), c.name, c.detail)
				if c.level == levelFail {
					failures++
				}
			}
			if failures > 0 {
				return fmt.Errorf("%d check(s) failed", failures)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&instance, "instance", "",
		"also verify this instance exists and report where it lives")
	ampCfg.register(cmd)
	return cmd
}

func repositoryChecks() []check {
	if global.repoPath == "" {
		return []check{{"repository", "no --repo given; skipping repository checks", levelWarn}}
	}

	r, err := repo.Open(global.repoPath)
	if err != nil {
		return []check{{"repository", err.Error(), levelFail}}
	}

	out := []check{{
		"repository",
		fmt.Sprintf("%s (%s, %s level %d)", r.Root(), r.Config().HashAlgo,
			r.Config().CompressionAlgo, r.Config().CompressionLevel),
		levelOK,
	}}

	snaps, err := r.ListSnapshots()
	if err != nil {
		out = append(out, check{"snapshots", err.Error(), levelFail})
	} else {
		detail := fmt.Sprintf("%d snapshot(s)", len(snaps))
		lvl := levelOK
		var partial int
		for _, m := range snaps {
			if m.State == repo.StatePartial {
				partial++
			}
		}
		if partial > 0 {
			detail += fmt.Sprintf(", %d partial", partial)
			lvl = levelWarn
		}
		out = append(out, check{"snapshots", detail, lvl})
	}

	space, err := r.Space()
	if err != nil {
		out = append(out, check{"free space", err.Error(), levelWarn})
		return out
	}
	detail := fmt.Sprintf("%s free of %s (%.0f%% used)",
		humanBytes(int64(space.AvailableBytes)), humanBytes(int64(space.TotalBytes)),
		space.UsedPercent())
	lvl := levelOK
	switch {
	case space.UsedPercent() >= 95:
		lvl = levelFail
		detail += " — too full to back up into safely"
	case space.UsedPercent() >= 85:
		lvl = levelWarn
		detail += " — tight; exclude what does not belong in the backup"
	}
	out = append(out, check{"free space", detail, lvl})

	// A repository sharing a disk with unrelated services is worth naming
	// explicitly: filling it takes them down too, not just the backups.
	if lvl != levelOK {
		out = append(out, check{"reserve", fmt.Sprintf(
			"a run refuses to start unless %s stays free (--reserve)",
			humanBytes(2<<30)), levelOK})
	}
	return out
}

func ampChecks(ctx context.Context, cfg *ampFlags, instance string) []check {
	if !cfg.configured() {
		return []check{{"AMP panel", "no --amp-url given; quiescing and instance lookup are unavailable", levelWarn}}
	}

	client, err := cfg.client()
	if err != nil {
		return []check{{"AMP panel", err.Error(), levelFail}}
	}
	if err := client.Login(ctx); err != nil {
		return []check{{"AMP login", err.Error(), levelFail}}
	}
	out := []check{{"AMP login", fmt.Sprintf("authenticated as %s at %s", cfg.user, cfg.url), levelOK}}

	spec, err := client.GetAPISpec(ctx)
	if err != nil {
		out = append(out, check{"AMP API spec", err.Error(), levelWarn})
	} else {
		var missing []string
		for _, m := range requiredMethods {
			if !amp.HasMethod(spec, m[0], m[1]) {
				missing = append(missing, m[0]+"."+m[1])
			}
		}
		if len(missing) > 0 {
			out = append(out, check{"AMP API spec",
				"this AMP build is missing " + strings.Join(missing, ", "), levelFail})
		} else {
			out = append(out, check{"AMP API spec",
				fmt.Sprintf("%d modules, all required methods present", len(spec)), levelOK})
		}
	}

	if status, err := client.GetStatus(ctx); err != nil {
		out = append(out, check{"application state", err.Error(), levelWarn})
	} else {
		detail := status.State.String()
		if !status.State.Running() {
			detail += " — quiescing will be skipped"
		}
		out = append(out, check{"application state", detail, levelOK})
	}

	instances, err := client.GetLocalInstances(ctx)
	if err != nil {
		out = append(out, check{"instances", err.Error(), levelWarn})
		return out
	}
	var names []string
	for _, in := range instances {
		names = append(names, in.InstanceName)
	}
	out = append(out, check{"instances", strings.Join(names, ", "), levelOK})

	if instance != "" {
		root, err := resolveRoot(ctx, client, instance)
		if err != nil {
			out = append(out, check{"instance " + instance, err.Error(), levelFail})
			return out
		}
		lvl := levelOK
		detail := root
		if _, err := os.Stat(root); err != nil {
			lvl = levelFail
			detail = fmt.Sprintf("%s — but this process cannot read it: %v", root, err)
		}
		out = append(out, check{"instance " + instance, detail, lvl})
	}
	return out
}
