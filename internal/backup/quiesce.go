package backup

import "context"

// Quiescer puts an application into a state where its hot files stop changing,
// and — crucially — always lets it go again.
//
// The contract is deliberately blunt about failure: Release must be safe to
// call twice, must be safe to call after a failed Quiesce, and must do
// everything it can to succeed. A Minecraft server left with save-off after a
// crashed backup accumulates unsaved world state until it is restarted, and
// that is a far worse outcome than a missed snapshot.
type Quiescer interface {
	// Name identifies the strategy in logs and manifests.
	Name() string
	// Quiesce blocks until the application has flushed and stopped writing.
	Quiesce(ctx context.Context) error
	// Release returns the application to normal operation.
	Release(ctx context.Context) error
}

// NoQuiesce is used when nothing needs to be held still: the instance is
// stopped, or the caller asked for a cold-only run.
type NoQuiesce struct{ Reason string }

func (n NoQuiesce) Name() string {
	if n.Reason != "" {
		return "none (" + n.Reason + ")"
	}
	return "none"
}

func (NoQuiesce) Quiesce(context.Context) error { return nil }
func (NoQuiesce) Release(context.Context) error { return nil }
