// Package quiesce holds an application still while its live files are read.
//
// For Minecraft that means save-off, save-all flush, waiting for the server to
// confirm it has written everything, and save-on afterwards. The waiting is
// the point: sleeping a fixed interval and hoping is how half-written region
// files get into backups.
package quiesce

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
)

// ConsoleClient is the slice of the AMP API a console quiescer needs.
type ConsoleClient interface {
	GetStatus(ctx context.Context) (amp.Status, error)
	SendConsoleMessage(ctx context.Context, message string) error
	GetUpdates(ctx context.Context) (amp.Updates, error)
}

// Defaults for a Minecraft Java server.
const (
	DefaultSaveOff         = "save-off"
	DefaultSaveFlush       = "save-all flush"
	DefaultSaveOn          = "save-on"
	DefaultConfirmTimeout  = 60 * time.Second
	DefaultPollInterval    = 250 * time.Millisecond
	DefaultWatchdogTimeout = 120 * time.Second
)

// DefaultConfirmPattern matches the line a vanilla or modded server prints once
// a flush has completed.
var DefaultConfirmPattern = regexp.MustCompile(`(?i)Saved the game|Saved the world|ThreadedAnvilChunkStorage.*[Ss]aved`)

// Config configures a Console quiescer.
type Config struct {
	SaveOff   string
	SaveFlush string
	SaveOn    string

	// ConfirmPattern identifies the console line that means the flush finished.
	ConfirmPattern *regexp.Regexp
	// ConfirmTimeout bounds the wait for that line. On timeout the quiesce
	// fails and saving is turned back on; a backup of possibly-torn region
	// files is not worth taking.
	ConfirmTimeout time.Duration
	PollInterval   time.Duration

	// WatchdogTimeout is the outer bound after which save-on is sent
	// regardless of what the caller is doing. This is the safety net that
	// keeps a hung or crashed backup from leaving a server unable to save.
	WatchdogTimeout time.Duration

	// SkipIfStopped makes quiescing a no-op when the application is not
	// running. A stopped server is already still.
	SkipIfStopped bool

	// Log receives progress and, more importantly, warnings.
	Log func(format string, args ...any)
}

func (c *Config) applyDefaults() {
	if c.SaveOff == "" {
		c.SaveOff = DefaultSaveOff
	}
	if c.SaveFlush == "" {
		c.SaveFlush = DefaultSaveFlush
	}
	if c.SaveOn == "" {
		c.SaveOn = DefaultSaveOn
	}
	if c.ConfirmPattern == nil {
		c.ConfirmPattern = DefaultConfirmPattern
	}
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = DefaultConfirmTimeout
	}
	if c.PollInterval <= 0 {
		c.PollInterval = DefaultPollInterval
	}
	if c.WatchdogTimeout <= 0 {
		c.WatchdogTimeout = DefaultWatchdogTimeout
	}
	if c.Log == nil {
		c.Log = func(string, ...any) {}
	}
}

// Console quiesces an application through AMP's console.
type Console struct {
	client ConsoleClient
	cfg    Config

	mu           sync.Mutex
	held         bool
	skipped      bool
	stopWatchdog context.CancelFunc
	watchdogDone chan struct{}
}

// NewConsole builds a console quiescer.
func NewConsole(client ConsoleClient, cfg Config) *Console {
	cfg.applyDefaults()
	return &Console{client: client, cfg: cfg}
}

// Name identifies the strategy.
func (c *Console) Name() string { return "console" }

// Quiesce disables saving and waits for the application to confirm it flushed.
func (c *Console) Quiesce(ctx context.Context) error {
	c.mu.Lock()
	if c.held || c.skipped {
		c.mu.Unlock()
		return errors.New("quiesce: already quiesced")
	}
	c.mu.Unlock()

	if c.cfg.SkipIfStopped {
		status, err := c.client.GetStatus(ctx)
		if err != nil {
			return fmt.Errorf("quiesce: check application state: %w", err)
		}
		if !status.State.Running() {
			c.cfg.Log("application is %s; nothing to quiesce", status.State)
			c.mu.Lock()
			c.skipped = true
			c.mu.Unlock()
			return nil
		}
	}

	// Drain anything the console produced earlier, so a "Saved the game" from
	// a previous autosave cannot be mistaken for confirmation of this flush.
	if _, err := c.client.GetUpdates(ctx); err != nil {
		return fmt.Errorf("quiesce: drain console: %w", err)
	}

	if err := c.client.SendConsoleMessage(ctx, c.cfg.SaveOff); err != nil {
		return fmt.Errorf("quiesce: send %q: %w", c.cfg.SaveOff, err)
	}

	// From this point saving is off, so every path out must turn it back on.
	c.mu.Lock()
	c.held = true
	c.mu.Unlock()
	c.startWatchdog()

	if err := c.client.SendConsoleMessage(ctx, c.cfg.SaveFlush); err != nil {
		return fmt.Errorf("quiesce: send %q: %w", c.cfg.SaveFlush, err)
	}
	if err := c.waitForFlush(ctx); err != nil {
		return err
	}
	return nil
}

// waitForFlush polls the console until the confirmation line appears.
func (c *Console) waitForFlush(ctx context.Context) error {
	deadline := time.Now().Add(c.cfg.ConfirmTimeout)
	ticker := time.NewTicker(c.cfg.PollInterval)
	defer ticker.Stop()

	for {
		updates, err := c.client.GetUpdates(ctx)
		if err != nil {
			return fmt.Errorf("quiesce: poll console: %w", err)
		}
		for _, e := range updates.ConsoleEntries {
			if c.cfg.ConfirmPattern.MatchString(e.Contents) {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("quiesce: %s did not confirm within %s "+
				"(looking for %s in the console); refusing to read a world that "+
				"may still be mid-write",
				c.cfg.SaveFlush, c.cfg.ConfirmTimeout, c.cfg.ConfirmPattern)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Release turns saving back on. It is safe to call more than once, and safe to
// call after a failed Quiesce.
func (c *Console) Release(ctx context.Context) error {
	c.mu.Lock()
	held := c.held
	c.held = false
	c.skipped = false
	stop := c.stopWatchdog
	done := c.watchdogDone
	c.stopWatchdog = nil
	c.watchdogDone = nil
	c.mu.Unlock()

	if stop != nil {
		stop()
		<-done
	}
	if !held {
		return nil
	}
	if err := c.client.SendConsoleMessage(ctx, c.cfg.SaveOn); err != nil {
		return fmt.Errorf("quiesce: send %q: %w", c.cfg.SaveOn, err)
	}
	return nil
}

// startWatchdog arms the independent save-on. It exists because every other
// safeguard in this file runs on the same goroutine as the backup: if that
// goroutine wedges, only something outside it can rescue the server.
func (c *Console) startWatchdog() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	c.mu.Lock()
	c.stopWatchdog = cancel
	c.watchdogDone = done
	c.mu.Unlock()

	go func() {
		defer close(done)
		timer := time.NewTimer(c.cfg.WatchdogTimeout)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		c.mu.Lock()
		stillHeld := c.held
		c.held = false
		c.mu.Unlock()
		if !stillHeld {
			return
		}

		c.cfg.Log("WARNING: still quiesced after %s; sending %q to protect the server",
			c.cfg.WatchdogTimeout, c.cfg.SaveOn)

		// A fresh context: whatever wedged the backup may have been a
		// cancellation, and this send must happen anyway.
		sendCtx, sendCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer sendCancel()
		if err := c.client.SendConsoleMessage(sendCtx, c.cfg.SaveOn); err != nil {
			c.cfg.Log("ERROR: watchdog could not re-enable saving: %v. "+
				"Run %q in the AMP console by hand.", err, c.cfg.SaveOn)
		}
	}()
}

// EnsureSaveOn sends save-on unconditionally. A daemon calls this at startup so
// that a server left quiesced by a process that died is recovered rather than
// silently accumulating unsaved world state.
func EnsureSaveOn(ctx context.Context, client ConsoleClient, command string) error {
	if command == "" {
		command = DefaultSaveOn
	}
	return client.SendConsoleMessage(ctx, command)
}
