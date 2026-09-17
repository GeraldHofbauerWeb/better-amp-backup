package cli

import (
	"net"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// Socket activation is three environment variables and a well-known
// descriptor number. Getting any of it subtly wrong means the daemon opens its
// own socket instead, with its own group, and nginx answers 502 -- which is
// exactly how it was discovered in the first place.
//
// It has to be tested in a child process, the way systemd does it. Dup'ing a
// descriptor onto number 3 inside this process would land on one the Go
// runtime is already using, and every socket in the test binary stops working
// -- which is what the first attempt at this test did.
func TestInheritedListenerTakesOverTheSystemdSocket(t *testing.T) {
	if os.Getenv("AMPBB_ACTIVATION_CHILD") != "" {
		activationChild(t)
		return
	}

	path := t.TempDir() + "/activation.sock"
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	file, err := listener.(*net.UnixListener).File()
	if err != nil {
		t.Fatalf("File: %v", err)
	}
	defer file.Close()

	// ExtraFiles[0] becomes descriptor 3 in the child, which is the number
	// systemd uses.
	child := exec.Command(os.Args[0], "-test.run=TestInheritedListenerTakesOverTheSystemdSocket", "-test.v")
	child.ExtraFiles = []*os.File{file}
	child.Env = append(os.Environ(),
		"AMPBB_ACTIVATION_CHILD=1",
		"AMPBB_ACTIVATION_PATH="+path,
		"LISTEN_FDS=1",
		"LISTEN_FDNAMES=activation.sock",
	)
	// LISTEN_PID is the child's, which it fills in for itself: it cannot be
	// known here.
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("the child failed: %v\n%s", err, output)
	}
}

// activationChild runs inside the spawned process, with the socket on
// descriptor 3.
func activationChild(t *testing.T) {
	path := os.Getenv("AMPBB_ACTIVATION_PATH")
	os.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))

	listener, err := inheritedListener()
	if err != nil {
		t.Fatalf("inheritedListener: %v", err)
	}
	if listener == nil {
		t.Fatal("no listener was taken over")
	}
	defer listener.Close()

	if got := listener.Addr().String(); got != path {
		t.Fatalf("address = %q, want %q", got, path)
	}

	// And it actually accepts, which is the only proof that matters.
	go func() {
		conn, err := net.Dial("unix", path)
		if err == nil {
			conn.Close()
		}
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	conn.Close()
}

// Every one of these means "not activated", and the daemon has to fall back to
// opening its own rather than failing or taking over a stranger's descriptor.
func TestInheritedListenerIgnoresWhatIsNotForUs(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"nothing set", map[string]string{}},
		{"another process's variables", map[string]string{
			"LISTEN_PID": strconv.Itoa(os.Getpid() + 1), "LISTEN_FDS": "1"}},
		{"no descriptors", map[string]string{
			"LISTEN_PID": strconv.Itoa(os.Getpid()), "LISTEN_FDS": "0"}},
		{"unparseable count", map[string]string{
			"LISTEN_PID": strconv.Itoa(os.Getpid()), "LISTEN_FDS": "lots"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("LISTEN_PID", "")
			t.Setenv("LISTEN_FDS", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			listener, err := inheritedListener()
			if err != nil {
				t.Fatalf("err = %v, want none -- this is a fallback, not a failure", err)
			}
			if listener != nil {
				listener.Close()
				t.Error("a listener was taken over that was not ours")
			}
		})
	}
}

// More than one socket means the unit was configured for something this daemon
// does not do, and picking the first would be a guess.
func TestSeveralSocketsAreRefused(t *testing.T) {
	t.Setenv("LISTEN_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("LISTEN_FDS", "2")
	if _, err := inheritedListener(); err == nil {
		t.Error("two sockets were accepted")
	}
}

// The daemon can restore a world and stop a server.
func TestListenOnRefusesAPublicInterface(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	for _, address := range []string{"0.0.0.0:8977", ":8977", "[::]:8977"} {
		if _, _, err := listenOn(address); err == nil {
			t.Errorf("%q was accepted", address)
		}
	}
}

func TestListenOnOpensAUnixSocketWithTheRightMode(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	path := t.TempDir() + "/api.sock"

	listener, cleanup, err := listenOn("unix:" + path)
	if err != nil {
		t.Fatalf("listenOn: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o660 {
		t.Errorf("mode = %04o, want 0660", got)
	}
	listener.Close()
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the socket was left behind; the next start would fail with 'address already in use'")
	}
}

// A socket left by a killed process must not block the next start for ever.
func TestAStaleSocketIsReplaced(t *testing.T) {
	t.Setenv("LISTEN_PID", "")
	t.Setenv("LISTEN_FDS", "")
	path := t.TempDir() + "/api.sock"

	first, _, err := listenOn("unix:" + path)
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener but leave the file, the way a SIGKILL would.
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the stale socket is not there, so this proves nothing: %v", err)
	}

	second, cleanup, err := listenOn("unix:" + path)
	if err != nil {
		t.Fatalf("a stale socket blocked the start: %v", err)
	}
	second.Close()
	cleanup()
}
