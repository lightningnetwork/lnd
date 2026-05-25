// Package sdnotify is a minimal in-tree replacement for the SdNotify call
// from github.com/coreos/go-systemd/daemon. lnd uses only that single
// function (out of the ~12 the upstream module exposes) to talk to systemd
// for "Type=notify" service supervision.
//
// The wire protocol is trivial: write the state string to the
// AF_UNIX/SOCK_DGRAM socket whose path is in the NOTIFY_SOCKET environment
// variable. When the variable is unset, the call is a no-op. See
// https://www.freedesktop.org/software/systemd/man/sd_notify.html for the
// upstream contract.
package sdnotify

import (
	"net"
	"os"
)

// SdNotifyReady tells the service manager the daemon has finished
// startup. It is the standard "Type=notify" handshake.
const SdNotifyReady = "READY=1"

// SdNotifyStopping tells the service manager the daemon is beginning a
// graceful shutdown.
const SdNotifyStopping = "STOPPING=1"

// SdNotify sends a notification to the systemd service manager.
//
// The first return value reports whether anything was sent: it is false
// (with a nil error) when NOTIFY_SOCKET is unset, which is the common
// case outside of a systemd "Type=notify" unit. Callers can therefore
// ignore the false-nil case the same way the upstream
// coreos/go-systemd/daemon package does.
//
// When unsetEnvironment is true, the NOTIFY_SOCKET variable is cleared
// after the connect attempt so child processes do not inherit it. This
// matches the upstream semantics that the daemon package exposes.
func SdNotify(unsetEnvironment bool, state string) (bool, error) {
	socketPath := os.Getenv("NOTIFY_SOCKET")
	if socketPath == "" {
		return false, nil
	}

	// Per sd_notify(3) the variable is single-use; callers can ask us
	// to clear it so it does not leak to forked children.
	if unsetEnvironment {
		defer func() { _ = os.Unsetenv("NOTIFY_SOCKET") }()
	}

	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{
		Name: socketPath,
		Net:  "unixgram",
	})
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(state)); err != nil {
		return false, err
	}
	return true, nil
}
