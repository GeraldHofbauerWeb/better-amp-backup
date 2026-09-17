package amp

import (
	"context"
	"fmt"
	"strings"
	"time"
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

// coreModule holds the methods every AMP endpoint has, controller or instance.
const coreModule = "Core"

// Control names the methods this AMP build exposes for the application's
// lifecycle, as discovered from a permission-filtered API spec.
//
// An empty field means the caller may not do that thing. GetAPISpec omits what
// the session is not allowed to call, which makes the spec a capability list
// as well as a compatibility check -- so this is how the web interface decides
// whether to show a stop button, rather than by trying and reporting a failure.
type Control struct {
	Start   string
	Stop    string
	Restart string
}

// ControlMethods reads the lifecycle methods out of an API spec.
func ControlMethods(spec map[string]map[string]any) Control {
	var ctl Control
	for _, candidate := range []struct {
		field  *string
		module string
		method string
	}{
		{&ctl.Start, coreModule, "Start"},
		{&ctl.Stop, coreModule, "Stop"},
		{&ctl.Restart, coreModule, "Restart"},
	} {
		if HasMethod(spec, candidate.module, candidate.method) {
			*candidate.field = candidate.module + "." + candidate.method
		}
	}
	return ctl
}

// CanStart and CanStop report whether the spec this Control came from allowed
// the call at all.
func (c Control) CanStart() bool { return c.Start != "" }
func (c Control) CanStop() bool  { return c.Stop != "" }

// StartApplication asks AMP to start the application.
//
// The method name comes from ctl rather than being written here, because a
// spec that does not list it means this session may not make the call --
// refusing locally gives a better error than AMP's generic one, and does not
// depend on guessing what a future build renamed it to.
func (c *Client) StartApplication(ctx context.Context, ctl Control) error {
	return c.control(ctx, ctl.Start, "start")
}

// StopApplication asks AMP to stop the application. It returns as soon as AMP
// accepts the request; the application is still running at that point.
func (c *Client) StopApplication(ctx context.Context, ctl Control) error {
	return c.control(ctx, ctl.Stop, "stop")
}

// RestartApplication asks AMP to restart the application.
func (c *Client) RestartApplication(ctx context.Context, ctl Control) error {
	return c.control(ctx, ctl.Restart, "restart")
}

func (c *Client) control(ctx context.Context, qualified, verb string) error {
	if qualified == "" {
		return fmt.Errorf("amp: this session may not %s the application", verb)
	}
	module, method, ok := strings.Cut(qualified, ".")
	if !ok {
		return fmt.Errorf("amp: malformed control method %q", qualified)
	}
	return c.Call(ctx, module, method, nil, nil)
}

// WaitForState polls until the application reaches want, or ctx is done.
//
// It exists because stopping is not an event but a process: StopApplication
// returns while the server is still saving its world, and anything that writes
// into the instance directory before it has finished is corrupting a backup it
// is supposed to be restoring.
func (c *Client) WaitForState(ctx context.Context, want State, poll time.Duration) error {
	if poll <= 0 {
		poll = time.Second
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	last := State(-1)
	for {
		status, err := c.GetStatus(ctx)
		switch {
		case err == nil:
			if status.State == want {
				return nil
			}
			last = status.State
		case ctx.Err() != nil:
			// The deadline landed mid-request. Report what we were waiting
			// for, not the transport error, which says nothing useful.
			return waitedInVain(want, last, ctx.Err())
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return waitedInVain(want, last, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitedInVain(want, last State, cause error) error {
	seen := "nothing yet"
	if last >= 0 {
		seen = last.String()
	}
	return fmt.Errorf("amp: gave up waiting for %s; last saw %s: %w", want, seen, cause)
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
