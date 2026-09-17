package amp

import (
	"context"
	"fmt"
	"strings"
)

// State mirrors AMP's application state enum. Only the values a backup cares
// about are named; the rest are reported numerically.
type State int

const (
	StateStopped     State = 0
	StatePreStart    State = 5
	StateConfiguring State = 7
	StateStarting    State = 10
	StateReady       State = 20
	StateRestarting  State = 30
	StateStopping    State = 40
	StatePendingUser State = 45
	StateMaintenance State = 50
	StateIndeterm    State = 60
)

func (s State) String() string {
	switch s {
	case StateStopped:
		return "stopped"
	case StatePreStart:
		return "pre-start"
	case StateConfiguring:
		return "configuring"
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateRestarting:
		return "restarting"
	case StateStopping:
		return "stopping"
	case StatePendingUser:
		return "pending user input"
	case StateMaintenance:
		return "maintenance"
	case StateIndeterm:
		return "indeterminate"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// Running reports whether the application is up and serving. Only in this
// state does quiescing mean anything; a stopped instance is already still.
func (s State) Running() bool { return s == StateReady }

// Status is the subset of Core.GetStatus this tool uses.
type Status struct {
	State  State  `json:"State"`
	Uptime string `json:"Uptime"`
}

// GetStatus reports the application state.
func (c *Client) GetStatus(ctx context.Context) (Status, error) {
	var s Status
	if err := c.Call(ctx, "Core", "GetStatus", nil, &s); err != nil {
		return Status{}, err
	}
	return s, nil
}

// SendConsoleMessage types a line into the application console. This is how
// save-off, save-all flush and save-on are delivered: AMP-managed instances
// usually have RCON disabled, and the console is always available.
func (c *Client) SendConsoleMessage(ctx context.Context, message string) error {
	return c.Call(ctx, "Core", "SendConsoleMessage", map[string]any{"message": message}, nil)
}

// ConsoleEntry is one line of console output.
type ConsoleEntry struct {
	Timestamp string `json:"Timestamp"`
	Source    string `json:"Source"`
	Type      string `json:"Type"`
	Contents  string `json:"Contents"`
}

// Updates is the polling payload AMP returns for console output and state.
type Updates struct {
	ConsoleEntries []ConsoleEntry `json:"ConsoleEntries"`
	Status         struct {
		State State `json:"State"`
	} `json:"Status"`
}

// GetUpdates drains everything the console produced since the previous call
// for this session. It is a drain, not a snapshot: a line is delivered once,
// so a poll loop must inspect every batch it receives.
func (c *Client) GetUpdates(ctx context.Context) (Updates, error) {
	var u Updates
	if err := c.Call(ctx, "Core", "GetUpdates", nil, &u); err != nil {
		return Updates{}, err
	}
	return u, nil
}

// Instance describes one AMP instance as reported by ADSModule.
type Instance struct {
	InstanceID   string `json:"InstanceID"`
	InstanceName string `json:"InstanceName"`
	FriendlyName string `json:"FriendlyName"`
	Module       string `json:"Module"`
	Running      bool   `json:"Running"`
	Suspended    bool   `json:"Suspended"`
	// BasePath is where the instance's files live. The field name has varied
	// between AMP versions, so several spellings are accepted.
	BasePath  string `json:"BasePath"`
	Path      string `json:"Path"`
	Datastore string `json:"Datastore"`
}

// Directory returns the instance directory, preferring whichever field this
// AMP version populated. An empty result means the caller must be told the
// path explicitly rather than have one guessed.
func (i Instance) Directory() string {
	for _, candidate := range []string{i.BasePath, i.Path} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

// GetLocalInstances lists the instances managed by this controller.
//
// Discovery goes through the API rather than through a hard-coded
// /home/amp/.ampdata path because AMP 2.8 can spread instances across several
// datastores, and because the datastore root is configurable.
func (c *Client) GetLocalInstances(ctx context.Context) ([]Instance, error) {
	var out []Instance
	if err := c.Call(ctx, "ADSModule", "GetLocalInstances", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveInstanceRoot asks AMP where an instance lives, so that a datastore
// layout this tool has never seen still works and no path is guessed.
//
// The name is matched against both the instance name and the friendly name,
// case-insensitively, because AMP shows the friendly one in the panel and the
// other on disk -- and the two routinely differ.
func (c *Client) ResolveInstanceRoot(ctx context.Context, instance string) (string, error) {
	instances, err := c.GetLocalInstances(ctx)
	if err != nil {
		return "", fmt.Errorf("listing instances: %w", err)
	}
	var names []string
	for _, in := range instances {
		names = append(names, in.InstanceName)
		if !strings.EqualFold(in.InstanceName, instance) && !strings.EqualFold(in.FriendlyName, instance) {
			continue
		}
		dir := in.Directory()
		if dir == "" {
			return "", fmt.Errorf("AMP reported instance %q but no directory for it; pass --root explicitly",
				instance)
		}
		return dir, nil
	}
	return "", fmt.Errorf("AMP does not know an instance named %q (it lists: %s)",
		instance, strings.Join(names, ", "))
}

// GetAPISpec returns the live API surface of this AMP build. It is what
// `amp-bb doctor` uses to check that the methods this tool relies on exist,
// instead of discovering a rename halfway through a backup.
func (c *Client) GetAPISpec(ctx context.Context) (map[string]map[string]any, error) {
	var spec map[string]map[string]any
	if err := c.Call(ctx, "Core", "GetAPISpec", nil, &spec); err != nil {
		return nil, err
	}
	return spec, nil
}

// HasMethod reports whether a module and method are present in a spec.
func HasMethod(spec map[string]map[string]any, module, method string) bool {
	m, ok := spec[module]
	if !ok {
		return false
	}
	_, ok = m[method]
	return ok
}
