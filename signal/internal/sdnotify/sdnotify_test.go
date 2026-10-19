package sdnotify

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// socketPath returns a short temporary path that fits in sockaddr_un on
// platforms with small sun_path fields.
func socketPath(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("", "sdnotify-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })

	return filepath.Join(dir, "n.sock")
}

// TestSdNotifyNoSocket asserts the common case where NOTIFY_SOCKET is
// unset: the function must report "nothing sent, no error", matching the
// upstream coreos/go-systemd contract that lnd's signal package relies on.
func TestSdNotifyNoSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")

	sent, err := SdNotify(false, SdNotifyReady)
	require.NoError(t, err)
	require.False(t, sent)
}

// TestSdNotifyDelivers verifies the full happy path by binding a temporary
// AF_UNIX/SOCK_DGRAM listener, pointing NOTIFY_SOCKET at it, calling
// SdNotify, and reading the datagram off the socket.
func TestSdNotifyDelivers(t *testing.T) {
	sockPath := socketPath(t)

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{
		Name: sockPath,
		Net:  "unixgram",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	t.Setenv("NOTIFY_SOCKET", sockPath)

	sent, err := SdNotify(false, SdNotifyReady)
	require.NoError(t, err)
	require.True(t, sent)

	buf := make([]byte, 64)
	n, _, err := conn.ReadFromUnix(buf)
	require.NoError(t, err)
	require.Equal(t, SdNotifyReady, string(buf[:n]))
}

// TestSdNotifyUnsetEnvironment confirms that the unsetEnvironment flag
// clears NOTIFY_SOCKET after the call. Forked children should not inherit
// the socket path.
func TestSdNotifyUnsetEnvironment(t *testing.T) {
	sockPath := socketPath(t)

	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{
		Name: sockPath,
		Net:  "unixgram",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	t.Setenv("NOTIFY_SOCKET", sockPath)

	_, err = SdNotify(true, SdNotifyStopping)
	require.NoError(t, err)

	require.Equal(t, "", os.Getenv("NOTIFY_SOCKET"))
}

// TestSdNotifyBadSocket asserts that a non-existent socket path surfaces
// as an error instead of being silently swallowed. lnd's caller uses the
// error to log a hint about systemd configuration.
func TestSdNotifyBadSocket(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "/nonexistent/path/that/will/not/connect")

	_, err := SdNotify(false, SdNotifyReady)
	require.Error(t, err)
}
