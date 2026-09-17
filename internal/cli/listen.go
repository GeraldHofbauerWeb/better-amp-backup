package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdFirstFD is the descriptor systemd passes an activated socket on.
// There is no library dependency here because the protocol is three
// environment variables and a well-known number.
const systemdFirstFD = 3

// inheritedListener returns the socket systemd created for us, if there is one.
//
// This is how nginx reaches the daemon without either of them being able to
// reach anything else of the other's. systemd makes the socket as root, gives
// it to nginx's group, and hands the daemon the open descriptor -- so the
// daemon still runs as the amp user and nginx still runs as www-data, and
// neither has to join a group that would also grant it the password file.
func inheritedListener() (net.Listener, error) {
	pid := os.Getenv("LISTEN_PID")
	if pid == "" {
		return nil, nil
	}
	// The variables are inherited by children too, so a mismatched pid means
	// they belong to a parent and are not ours to use.
	if pid != strconv.Itoa(os.Getpid()) {
		return nil, nil
	}
	count, err := strconv.Atoi(os.Getenv("LISTEN_FDS"))
	if err != nil || count < 1 {
		return nil, nil
	}
	if count > 1 {
		return nil, fmt.Errorf("systemd passed %d sockets; this daemon serves one", count)
	}

	name := os.Getenv("LISTEN_FDNAMES")
	if name == "" {
		name = "systemd"
	}
	file := os.NewFile(systemdFirstFD, name)
	if file == nil {
		return nil, errors.New("systemd said it passed a socket, but descriptor 3 is not open")
	}
	defer file.Close()

	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("taking over the socket from systemd: %w", err)
	}
	return listener, nil
}

// listenOn opens the socket the daemon serves on.
//
// Binding to anything but the loopback, a unix socket or one systemd handed us
// is refused. This process can restore a world and stop a server; putting it on
// a public interface without the panel's nginx in front is not a
// configuration, it is an accident.
func listenOn(address string) (net.Listener, func(), error) {
	// Socket activation wins over the flag: if systemd made a socket for us,
	// that is the one nginx is already pointed at.
	if listener, err := inheritedListener(); err != nil {
		return nil, nil, err
	} else if listener != nil {
		return listener, func() {}, nil
	}

	if path, ok := strings.CutPrefix(address, "unix:"); ok {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, nil, fmt.Errorf("creating the socket directory: %w", err)
		}
		// A socket left behind by a killed process would make this fail with
		// "address already in use" for ever.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("removing the stale socket: %w", err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			return nil, nil, err
		}
		// Only the owner and its group. Without socket activation the group is
		// the daemon's own, so this is reachable by nginx only if the two
		// share one -- which is why the unit uses an activated socket instead.
		if err := os.Chmod(path, 0o660); err != nil {
			listener.Close()
			return nil, nil, fmt.Errorf("setting the socket mode: %w", err)
		}
		return listener, func() { os.Remove(path) }, nil
	}

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("--listen %q is neither unix:/path nor host:port", address)
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "*" {
		return nil, nil, fmt.Errorf(
			"refusing to listen on %q: this process can restore a world and stop a server, "+
				"so it binds to the loopback or a unix socket and lets nginx do the rest", address)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, nil, err
	}
	return listener, func() {}, nil
}
