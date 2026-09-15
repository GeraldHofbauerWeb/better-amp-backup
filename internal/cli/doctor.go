package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
//
// ADSModule.GetLocalInstances is deliberately not in this list. It exists only
// on a controller (ADS); an instance endpoint answers "No such module loaded:
// 'ADSModule'" and is none the worse for it, because the only thing that
// method does here is look up an instance directory that --root can state
// outright. Requiring it made a correctly configured instance fail the check.
var requiredMethods = [][2]string{
	{"Core", "Login"},
	{"Core", "GetStatus"},
	{"Core", "GetUpdates"},
	{"Core", "SendConsoleMessage"},
}

// adsModule is the module a controller exposes and an instance does not.
const adsModule = "ADSModule"

func newDoctorCommand() *cobra.Command {
	var (
		ampCfg   ampFlags
		instance string
		root     string
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
			checks = append(checks, ampChecks(ctx, &ampCfg, instance, root)...)

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
	cmd.Flags().StringVar(&root, "root", "",
		"instance directory the backup will read; checked for readability")
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

func ampChecks(ctx context.Context, cfg *ampFlags, instance, root string) []check {
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
			// AMP filters this spec by what the caller may actually call, so
			// a missing method usually means a missing permission rather than
			// an old build -- and a role change made in the controller can
			// take a moment to reach the instance. Saying "this build is
			// missing" sent one operator hunting the wrong problem.
			out = append(out, check{"AMP API spec", fmt.Sprintf(
				"%s not available to user %s — grant the role Core.AppManagement "+
					"rights on this instance, or wait for a recent change to propagate",
				strings.Join(missing, ", "), cfg.user), levelFail})
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

	// Only a controller can list instances. Pointing at an instance directly
	// is the normal, and safer, way to run a backup, so its inability to
	// answer this is a fact about the endpoint, not a fault.
	isController := amp.HasMethod(spec, adsModule, "GetLocalInstances")
	switch {
	case !isController:
		out = append(out, check{"endpoint", fmt.Sprintf(
			"an instance, not a controller (no %s) — pass --root", adsModule), levelOK})
	default:
		if instances, err := client.GetLocalInstances(ctx); err != nil {
			out = append(out, check{"instances", err.Error(), levelWarn})
		} else {
			var names []string
			for _, in := range instances {
				names = append(names, in.InstanceName)
			}
			out = append(out, check{"instances", strings.Join(names, ", "), levelOK})
		}
	}

	// What the backup will actually read. An explicit --root is checked as
	// given; without one, a controller can still be asked where the instance
	// lives, and an instance endpoint can only say "tell me".
	if root == "" && instance != "" {
		if !isController {
			out = append(out, check{"instance " + instance,
				"cannot be looked up through an instance endpoint; pass --root", levelWarn})
			return out
		}
		var err error
		if root, err = resolveRoot(ctx, client, instance); err != nil {
			out = append(out, check{"instance " + instance, err.Error(), levelFail})
			return out
		}
	}
	if root != "" {
		name := "instance directory"
		if instance != "" {
			name = "instance " + instance
		}
		detail, lvl := rootDetail(root)
		out = append(out, check{name, detail, lvl})
	}
	return out
}

// rootDetail reports whether the process can actually read the instance
// directory. Stat alone is not enough: AMP keeps instances under a home
// directory that is routinely 0700, so a run started as the wrong user sees
// the path exist and then fails on the first read.
func rootDetail(root string) (string, level) {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Sprintf("%s — cannot be read: %v", root, err), levelFail
	}
	if !info.IsDir() {
		return fmt.Sprintf("%s — not a directory", root), levelFail
	}
	f, err := os.Open(root)
	if err != nil {
		return fmt.Sprintf("%s — cannot be opened: %v", root, err), levelFail
	}
	defer f.Close()
	// io.EOF only means the directory is empty, which is odd for an instance
	// but not an error; anything else is the permission problem being hunted.
	if _, err := f.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("%s — cannot be listed: %v", root, err), levelFail
	}
	return root, levelOK
}
