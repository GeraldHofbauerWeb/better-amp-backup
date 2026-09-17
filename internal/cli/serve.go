package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/auth"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/jobs"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/ops"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/schedule"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/settings"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/snaptree"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/state"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/web"
	"github.com/spf13/cobra"
)

// defaultSocket is where nginx expects to find us. A unix socket rather than a
// loopback port because it removes "any local user on this host can reach the
// restore API" as a category, and costs nothing to arrange.
const defaultSocket = "unix:/run/better-amp-backup/api.sock"

func newServeCommand() *cobra.Command {
	var (
		listen      string
		stateDir    string
		instance    string
		root        string
		instanceID  string
		panelURL    string
		scratchDir  string
		idleTimeout time.Duration
		ampCfg      ampFlags
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the backup daemon and the panel tab it serves",
		Long: "Keeps its own schedule, so it needs no systemd timer and no root.\n" +
			"nginx routes /amp-bb/ and /Plugins/AmpBB/ here; everything else goes to AMP.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if instance == "" {
				return errors.New("--instance is required")
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			r, err := openRepo()
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// The service account, for everything nobody is watching.
			var serviceClient *amp.Client
			if ampCfg.configured() {
				if serviceClient, err = ampCfg.client(); err != nil {
					return err
				}
			}
			if root == "" {
				if serviceClient == nil {
					return errors.New("pass --root, or --amp-url so the instance directory can be looked up")
				}
				if root, err = serviceClient.ResolveInstanceRoot(ctx, instance); err != nil {
					return err
				}
			}

			if stateDir == "" {
				stateDir = envOr("AMPBB_STATE_DIR", "/var/lib/better-amp-backup")
			}
			if err := os.MkdirAll(stateDir, 0o750); err != nil {
				return fmt.Errorf("creating the state directory: %w", err)
			}
			if scratchDir == "" {
				scratchDir = filepath.Join(stateDir, "restore")
			}

			seed := settings.Defaults()
			seed.Instance = settings.Instance{Name: instance, ID: instanceID, Root: root}
			settingsStore, err := settings.Open(filepath.Join(stateDir, "settings.json"), seed)
			if err != nil {
				return err
			}
			stateStore, err := state.Open(filepath.Join(stateDir, "state.json"))
			if err != nil {
				return err
			}

			runner := jobs.NewRunner(jobs.Options{})
			operations := &ops.Runner{
				Repo:        r,
				Settings:    settingsStore,
				ToolVersion: Version,
				ScratchDir:  scratchDir,
				Instance: func(context.Context) (*amp.Client, error) {
					if serviceClient == nil {
						return nil, errors.New("no AMP connection is configured; pass --amp-url and credentials")
					}
					return serviceClient, nil
				},
			}
			scheduler := schedule.New(settingsStore, stateStore, runner, operations, log, nil)
			// So that a restart does not postpone the next backup by a whole
			// interval, and so that taking over from the systemd timers
			// continues their rhythm instead of starting a new one.
			scheduler.LastSnapshotAt = func() time.Time {
				all, err := r.ListSnapshots()
				if err != nil {
					log.Warn("could not read the snapshot list for scheduling", "error", err)
					return time.Time{}
				}
				var newest time.Time
				for _, m := range all {
					if !strings.EqualFold(m.Instance, instance) {
						continue
					}
					if m.StartedAt.After(newest) {
						newest = m.StartedAt
					}
				}
				return newest
			}

			// A client that acts as whoever is looking at the tab. It goes
			// through the controller's instance proxy, which is the path the
			// panel itself uses once an instance is open.
			userClient := func(session string) (*amp.Client, error) {
				if panelURL == "" {
					return nil, errors.New("no --panel-url, so a panel session cannot be checked")
				}
				c, err := amp.NewWithSession(amp.SessionConfig{
					BaseURL: panelURL, Session: session,
					Timeout: ampCfg.timeout, InsecureSkipVerify: ampCfg.insecure,
				})
				if err != nil {
					return nil, err
				}
				if instanceID != "" {
					return c.ForInstance(instanceID), nil
				}
				return c, nil
			}

			handler, err := web.NewHandler(web.Deps{
				Repo: r, Settings: settingsStore, State: stateStore,
				Jobs: runner, Ops: operations,
				Trees:      snaptree.NewCache(0, 0, 0),
				Sessions:   auth.NewStore(0, 0, nil),
				Validator:  auth.AMPValidator{Dial: userClient},
				UserClient: userClient,
				ServiceClient: func(context.Context) (*amp.Client, error) {
					if serviceClient == nil {
						return nil, errors.New("no AMP connection is configured")
					}
					return serviceClient, nil
				},
				Scheduler: scheduler,
				Version:   Version,
				Log:       log,
			})
			if err != nil {
				return err
			}

			listener, cleanup, err := listenOn(listen)
			if err != nil {
				return err
			}
			defer cleanup()

			server := &http.Server{
				Handler: handler,
				// WriteTimeout has to stay zero or the event stream dies at
				// the deadline. The stream is bounded by its own heartbeat and
				// by the request context instead.
				ReadHeaderTimeout: 10 * time.Second,
				IdleTimeout:       idleTimeout,
				BaseContext:       func(net.Listener) context.Context { return ctx },
				ErrorLog:          nil,
			}

			go func() {
				if err := scheduler.Run(ctx); err != nil {
					log.Error("the scheduler stopped", "error", err)
				}
			}()

			log.Info("listening", "address", listener.Addr(), "instance", instance, "root", root,
				"repository", r.Root(), "state", stateDir)

			serverDone := make(chan error, 1)
			go func() { serverDone <- server.Serve(listener) }()

			select {
			case err := <-serverDone:
				if err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
			case <-ctx.Done():
			}

			log.Info("shutting down")
			// The running job first, and with a real grace period: a quiesced
			// backup unwinding is a backup sending save-on, and the difference
			// between waiting and not waiting is a Minecraft server left
			// unable to save.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := runner.Shutdown(shutdownCtx); err != nil {
				log.Warn("a job did not unwind in time", "error", err)
			}
			return server.Shutdown(shutdownCtx)
		},
	}

	cmd.Flags().StringVar(&listen, "listen", envOr("AMPBB_LISTEN", defaultSocket),
		"unix:/path/to.sock or host:port (env AMPBB_LISTEN)")
	cmd.Flags().StringVar(&stateDir, "state-dir", "",
		"where the schedule and the run history live (env AMPBB_STATE_DIR)")
	cmd.Flags().StringVar(&instance, "instance", envOr("AMPBB_INSTANCE", ""), "AMP instance name")
	cmd.Flags().StringVar(&root, "root", envOr("AMPBB_ROOT", ""),
		"instance directory; looked up through the AMP API when omitted")
	cmd.Flags().StringVar(&instanceID, "amp-instance-id", envOr("AMPBB_INSTANCE_ID", ""),
		"the instance's AMP GUID, used to reach it through the controller")
	cmd.Flags().StringVar(&panelURL, "panel-url", envOr("AMPBB_PANEL_URL", ""),
		"the controller's URL, for checking a visitor's panel session (env AMPBB_PANEL_URL)")
	cmd.Flags().StringVar(&scratchDir, "scratch-dir", "",
		"where a look-before-you-leap restore is written; defaults to a directory under --state-dir")
	cmd.Flags().DurationVar(&idleTimeout, "idle-timeout", 120*time.Second, "how long an idle connection is kept")
	ampCfg.register(cmd)
	return cmd
}
