package amp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp"
	"github.com/GeraldHofbauerWeb/better-amp-backup/internal/amp/fake"
)

const instanceID = "6b85d8b3-eaff-4722-ac97-52e4f4441948"

// The panel's session belongs to the controller and reaches an instance only
// through the proxy path. A client that addresses the controller directly
// talks to the wrong thing while appearing to work, so the routing is pinned
// down here.
func TestForInstanceRoutesThroughTheProxy(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.InstanceID = instanceID
	srv.RequireProxy = true
	srv.AddSession("panel-session")

	client, err := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "panel-session"})
	if err != nil {
		t.Fatalf("NewWithSession: %v", err)
	}

	if _, err := client.GetStatus(context.Background()); err == nil {
		t.Error("a direct call should have been refused while RequireProxy is set")
	}

	instance := client.ForInstance(instanceID)
	if _, err := instance.GetStatus(context.Background()); err != nil {
		t.Fatalf("proxied GetStatus: %v", err)
	}
	if got := srv.ProxiedCallLog(); len(got) != 1 || got[0] != "Core.GetStatus" {
		t.Errorf("proxied calls = %v, want one Core.GetStatus", got)
	}
}

func TestForInstanceRefusesAnUnknownInstance(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.InstanceID = instanceID
	srv.AddSession("panel-session")

	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "panel-session"})
	_, err := client.ForInstance("11111111-2222-3333-4444-555555555555").GetStatus(context.Background())
	if err == nil {
		t.Fatal("a call to an instance the controller does not host should fail")
	}
}

// A borrowed session cannot be renewed: there are no credentials to renew it
// with. Saying so plainly is the contract, and it is what stops the daemon
// from silently acting as somebody else once a person's session lapses.
func TestBorrowedSessionIsNotRenewed(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("panel-session")

	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "panel-session"})
	if _, err := client.GetStatus(context.Background()); err != nil {
		t.Fatalf("GetStatus with a good session: %v", err)
	}

	srv.RevokeSession("panel-session")
	_, err := client.GetStatus(context.Background())
	if !errors.Is(err, amp.ErrUnauthorized) {
		t.Errorf("err = %v, want ErrUnauthorized", err)
	}
	if srv.Logins != 0 {
		t.Errorf("the client logged in %d times; it has no credentials and must not try", srv.Logins)
	}
}

// GetAPISpec is filtered by the caller's permissions, which makes it a
// capability list. That is how the interface decides whether to offer a stop
// button, instead of offering one and reporting a failure afterwards.
func TestControlMethodsFollowThePermissionFilteredSpec(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("s")
	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "s"})

	spec, err := client.GetAPISpec(context.Background())
	if err != nil {
		t.Fatalf("GetAPISpec: %v", err)
	}
	ctl := amp.ControlMethods(spec)
	if !ctl.CanStart() || !ctl.CanStop() {
		t.Fatalf("a privileged spec should allow start and stop, got %+v", ctl)
	}
	if ctl.Stop != "Core.Stop" {
		t.Errorf("Stop = %q, want Core.Stop", ctl.Stop)
	}

	// Now the service account's view: console access, nothing else.
	srv.Spec = fake.BackupAccountSpec()
	spec, err = client.GetAPISpec(context.Background())
	if err != nil {
		t.Fatalf("GetAPISpec: %v", err)
	}
	ctl = amp.ControlMethods(spec)
	if ctl.CanStart() || ctl.CanStop() {
		t.Errorf("the backup account must not be able to start or stop, got %+v", ctl)
	}

	err = client.StopApplication(context.Background(), ctl)
	if err == nil || !strings.Contains(err.Error(), "may not stop") {
		t.Errorf("err = %v, want a local refusal naming the missing permission", err)
	}
}

func TestStopMovesTheApplicationOutOfReady(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("s")
	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "s"})
	ctx := context.Background()

	spec, _ := client.GetAPISpec(ctx)
	if err := client.StopApplication(ctx, amp.ControlMethods(spec)); err != nil {
		t.Fatalf("StopApplication: %v", err)
	}
	status, err := client.GetStatus(ctx)
	if err != nil {
		t.Fatalf("GetStatus: %v", err)
	}
	if status.State != amp.StateStopping {
		t.Errorf("State = %v, want stopping -- a stop is a process, not an event", status.State)
	}
}

// Nothing may write into the instance directory until the application has
// actually stopped, so the wait has to be real rather than a sleep.
func TestWaitForStateReturnsWhenTheStateArrives(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("s")
	srv.SetState(int(amp.StateStopping))
	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "s"})

	go func() {
		time.Sleep(30 * time.Millisecond)
		srv.SetState(int(amp.StateStopped))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.WaitForState(ctx, amp.StateStopped, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForState: %v", err)
	}
}

func TestWaitForStateGivesUpAndSaysWhatItSaw(t *testing.T) {
	srv := fake.New("user", "pw")
	defer srv.Close()
	srv.AddSession("s")
	client, _ := amp.NewWithSession(amp.SessionConfig{BaseURL: srv.URL, Session: "s"})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	err := client.WaitForState(ctx, amp.StateStopped, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	if !strings.Contains(err.Error(), "ready") {
		t.Errorf("error should name the state it actually saw, got: %v", err)
	}
}
