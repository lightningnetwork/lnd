package tor

import (
	"fmt"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestParseTorVersion is a series of tests for different version strings that
// check the correctness of determining whether they support creating v3 onion
// services through Tor control's port.
func TestParseTorVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version string
		valid   bool
	}{
		{
			version: "0.3.3.6",
			valid:   true,
		},
		{
			version: "0.3.3.7",
			valid:   true,
		},
		{
			version: "0.3.4.6",
			valid:   true,
		},
		{
			version: "0.4.3.6",
			valid:   true,
		},
		{
			version: "0.4.0.5",
			valid:   true,
		},
		{
			version: "1.3.3.6",
			valid:   true,
		},
		{
			version: "0.3.3.6-rc",
			valid:   true,
		},
		{
			version: "0.3.3.7-rc",
			valid:   true,
		},
		{
			version: "0.3.3.5-rc",
			valid:   false,
		},
		{
			version: "0.3.3.5",
			valid:   false,
		},
		{
			version: "0.3.2.6",
			valid:   false,
		},
		{
			version: "0.1.3.6",
			valid:   false,
		},
		{
			version: "0.0.6.3",
			valid:   false,
		},
	}

	for i, test := range tests {
		err := supportsV3(test.version)
		if test.valid != (err == nil) {
			t.Fatalf("test %d with version string %v failed: %v", i,
				test.version, err)
		}
	}
}

// testProxy emulates a Tor daemon and contains the info used for the tor
// controller to make connections.
type testProxy struct {
	// server is the proxy listener.
	server net.Listener

	// serverConn is the established connection from the server side.
	serverConn net.Conn

	// serverAddr is the tcp address the proxy is listening on.
	serverAddr string

	// clientConn is the established connection from the client side.
	clientConn *textproto.Conn
}

// cleanUp is used after each test to properly close the ports/connections.
func (tp *testProxy) cleanUp() {
	// Don't bother cleaning if there's no a server created.
	if tp.server == nil {
		return
	}

	if err := tp.clientConn.Close(); err != nil {
		log.Errorf("closing client conn got err: %v", err)
	}
	if err := tp.server.Close(); err != nil {
		log.Errorf("closing proxy server got err: %v", err)
	}
}

// createTestProxy creates a proxy server to listen on a random address,
// creates a server and a client connection, and initializes a testProxy using
// these params.
func createTestProxy(t *testing.T) *testProxy {
	// Set up the proxy to listen on a unique port.
	addr := fmt.Sprintf(":%d", nextAvailablePort())
	proxy, err := net.Listen("tcp", addr)
	require.NoError(t, err, "failed to create proxy")

	t.Logf("created proxy server to listen on address: %v", proxy.Addr())

	// Accept the connection inside a goroutine.
	serverChan := make(chan net.Conn, 1)
	go func(result chan net.Conn) {
		conn, err := proxy.Accept()
		require.NoError(t, err, "failed to accept")

		result <- conn
	}(serverChan)

	// Create the connection using tor controller.
	client, err := textproto.Dial("tcp", proxy.Addr().String())
	require.NoError(t, err, "failed to create connection")

	tc := &testProxy{
		server:     proxy,
		serverConn: <-serverChan,
		serverAddr: proxy.Addr().String(),
		clientConn: client,
	}

	t.Logf("server listening on %v, client listening on %v",
		tc.serverConn.LocalAddr(), tc.serverConn.RemoteAddr())

	return tc
}

// TestReadResponse constructs a series of possible responses returned by Tor
// and asserts the readResponse can handle them correctly.
func TestReadResponse(t *testing.T) {
	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Create a dummy tor controller.
	c := &Controller{conn: proxy.clientConn}

	testCase := []struct {
		name       string
		serverResp string

		// expectedReply is the reply we expect the readResponse to
		// return.
		expectedReply string

		// expectedCode is the code we expect the server to return.
		expectedCode int

		// returnedCode is the code we expect the readResponse to
		// return.
		returnedCode int

		// expectedErr is the error we expect the readResponse to
		// return.
		expectedErr error
	}{
		{
			// Test a simple response.
			name:          "succeed on 250",
			serverResp:    "250 OK\n",
			expectedReply: "OK",
			expectedCode:  250,
			returnedCode:  250,
			expectedErr:   nil,
		},
		{
			// Test a mid reply(-) response.
			name: "succeed on mid reply line",
			serverResp: "250-field=value\n" +
				"250 OK\n",
			expectedReply: "field=value\nOK",
			expectedCode:  250,
			returnedCode:  250,
			expectedErr:   nil,
		},
		{
			// Test a data reply(+) response.
			name: "succeed on data reply line",
			serverResp: "250+field=\n" +
				"line1\n" +
				"line2\n" +
				".\n" +
				"250 OK\n",
			expectedReply: "field=line1,line2\nOK",
			expectedCode:  250,
			returnedCode:  250,
			expectedErr:   nil,
		},
		{
			// Test a mixed reply response.
			name: "succeed on mixed reply line",
			serverResp: "250-field=value\n" +
				"250+field=\n" +
				"line1\n" +
				"line2\n" +
				".\n" +
				"250 OK\n",
			expectedReply: "field=value\nfield=line1,line2\nOK",
			expectedCode:  250,
			returnedCode:  250,
			expectedErr:   nil,
		},
		{
			// Test unexpected code.
			name:          "fail on codes not matched",
			serverResp:    "250 ERR\n",
			expectedReply: "ERR",
			expectedCode:  500,
			returnedCode:  250,
			expectedErr:   errCodeNotMatch,
		},
		{
			// Test short response error.
			name:          "fail on short response",
			serverResp:    "123\n250 OK\n",
			expectedReply: "",
			expectedCode:  250,
			returnedCode:  0,
			expectedErr: textproto.ProtocolError(
				"short line: 123"),
		},
		{
			// Test short response error.
			name:          "fail on invalid response",
			serverResp:    "250?OK\n",
			expectedReply: "",
			expectedCode:  250,
			returnedCode:  250,
			expectedErr: textproto.ProtocolError(
				"invalid line: 250?OK"),
		},
	}

	for _, tc := range testCase {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Let the server mocks a given response.
			_, err := server.Write([]byte(tc.serverResp))
			require.NoError(t, err, "server failed to write")

			// Read the response and checks all expectations
			// satisfied.
			code, reply, err := c.readResponse(tc.expectedCode)
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.returnedCode, code)
			require.Equal(t, tc.expectedReply, reply)

			// Check that the read buffer is cleaned.
			require.Zero(t, c.conn.R.Buffered(),
				"read buffer not empty")
		})
	}
}

// TestReconnectTCMustBeRunning checks that the tor controller must be running
// while calling Reconnect.
func TestReconnectTCMustBeRunning(t *testing.T) {
	// Create a dummy controller.
	c := &Controller{}

	// Reconnect should fail because the TC is not started.
	require.Equal(t, errTCNotStarted, c.Reconnect())

	// Set the started flag.
	c.started = 1

	// Set the stopped flag so the TC is stopped.
	c.stopped = 1

	// Reconnect should fail because the TC is stopped.
	require.Equal(t, errTCStopped, c.Reconnect())
}

// TestReconnectSucceed tests a reconnection will succeed when the tor
// controller is up and running.
func TestReconnectSucceed(t *testing.T) {
	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)

	// Create a tor controller and mark the controller as started.
	c := &Controller{
		conn:        proxy.clientConn,
		started:     1,
		controlAddr: proxy.serverAddr,
	}

	// Close the old conn before reconnection.
	require.NoError(t, proxy.serverConn.Close())

	// Accept the connection inside a goroutine. When the client makes a
	// reconnection, the messages flow is,
	// 1. the client sends the command PROTOCOLINFO to the server.
	// 2. the server responds with its protocol version.
	// 3. the client reads the response and sends the command AUTHENTICATE
	//    to the server
	// 4. the server responds with the authentication info.
	//
	// From the server's PoV, We need to mock two reads and two writes
	// inside the connection,
	// 1. read the command PROTOCOLINFO sent from the client.
	// 2. write protocol info so the client can read it.
	// 3. read the command AUTHENTICATE sent from the client.
	// 4. write auth challenge so the client can read it.
	go func() {
		// Accept the new connection.
		server, err := proxy.server.Accept()
		require.NoError(t, err, "failed to accept")

		t.Logf("server listening on %v, client listening on %v",
			server.LocalAddr(), server.RemoteAddr())

		// Read the protocol command from the client.
		buf := make([]byte, 65535)
		_, err = server.Read(buf)
		require.NoError(t, err)

		// Write the protocol info.
		resp := "250-PROTOCOLINFO 1\n" +
			"250-AUTH METHODS=NULL\n" +
			"250 OK\n"
		_, err = server.Write([]byte(resp))
		require.NoErrorf(t, err, "failed to write protocol info")

		// Read the auth info from the client.
		_, err = server.Read(buf)
		require.NoError(t, err)

		// Write the auth challenge.
		resp = "250 AUTHCHALLENGE SERVERHASH=fake\n"
		_, err = server.Write([]byte(resp))
		require.NoErrorf(t, err, "failed to write auth challenge")
	}()

	// Reconnect should succeed.
	require.NoError(t, c.Reconnect())

	// Check that the old connection is closed.
	_, err := proxy.clientConn.ReadLine()
	require.Contains(t, err.Error(), "use of closed network connection")

	// Check that the connection has been updated.
	require.NotEqual(t, proxy.clientConn, c.conn)
}

// TestParseTorReply tests that Tor replies are parsed correctly.
func TestParseTorReply(t *testing.T) {
	testCase := []struct {
		reply          string
		expectedParams map[string]string
	}{
		{
			// Test a regular reply.
			reply: `VERSION Tor="0.4.7.8"`,
			expectedParams: map[string]string{
				"Tor": "0.4.7.8",
			},
		},
		{
			// Test a reply with multiple values, one of them
			// containing spaces.
			reply: `AUTH METHODS=COOKIE,SAFECOOKIE,HASHEDPASSWORD` +
				` COOKIEFILE="/path/with/spaces/Tor Browser/c` +
				`ontrol_auth_cookie"`,
			expectedParams: map[string]string{
				"METHODS": "COOKIE,SAFECOOKIE,HASHEDPASSWORD",
				"COOKIEFILE": "/path/with/spaces/Tor Browser/" +
					"control_auth_cookie",
			},
		},
		{
			// Test a multiline reply.
			reply:          "ServiceID=id\r\nOK",
			expectedParams: map[string]string{"ServiceID": "id"},
		},
		{
			// Test a reply with invalid parameters.
			reply:          "AUTH =invalid",
			expectedParams: map[string]string{},
		},
		{
			// Test escaping arbitrary characters.
			reply: `PARAM="esca\ped \"doub\lequotes\""`,
			expectedParams: map[string]string{
				`PARAM`: `escaped "doublequotes"`,
			},
		},
		{
			// Test escaping backslashes. Each single backslash
			// should be removed, each double backslash replaced
			// with a single one. Note that the single backslash
			// before the space escapes the space character, so
			// there's two spaces in a row.
			reply: `PARAM="escaped \\ \ \\\\"`,
			expectedParams: map[string]string{
				`PARAM`: `escaped \  \\`,
			},
		},
	}

	for _, tc := range testCase {
		params := parseTorReply(tc.reply)
		require.Equal(t, tc.expectedParams, params)
	}
}

const (
	// listenerFormat is the format string that is used to generate local
	// listener addresses.
	listenerFormat = "127.0.0.1:%d"

	// defaultNodePort is the start of the range for listening ports of
	// harness nodes. Ports are monotonically increasing starting from this
	// number and are determined by the results of NextAvailablePort().
	defaultNodePort int = 10000

	// uniquePortFile is the name of the file that is used to store the
	// last port that was used by a node. This is used to make sure that
	// the same port is not used by multiple nodes at the same time. The
	// file is located in the temp directory of a system.
	uniquePortFile = "rpctest-port"
)

var (
	// portFileMutex is a mutex that is used to make sure that the port file
	// is not accessed by multiple goroutines of the same process at the
	// same time. This is used in conjunction with the lock file to make
	// sure that the port file is not accessed by multiple processes at the
	// same time either. So the lock file is to guard between processes and
	// the mutex is to guard between goroutines of the same process.
	portFileMutex sync.Mutex
)

// nextAvailablePort returns the first port that is available for listening by a
// new node, using a lock file to make sure concurrent access for parallel tasks
// on the same system don't re-use the same port.
//
// NOTE: This is a copy of `lntest/port`. Since `lnd/tor` is a submodule, it
// cannot import the port module `lntest/port` so we need to re-define it here.
func nextAvailablePort() int {
	portFileMutex.Lock()
	defer portFileMutex.Unlock()

	lockFile := filepath.Join(os.TempDir(), uniquePortFile+".lock")
	timeout := time.After(10 * time.Second)

	var (
		lockFileHandle *os.File
		err            error
	)
	for {
		// Attempt to acquire the lock file. If it already exists, wait
		// for a bit and retry.
		lockFileHandle, err = os.OpenFile(
			lockFile, os.O_CREATE|os.O_EXCL, 0600,
		)
		if err == nil {
			// Lock acquired.
			break
		}

		// Wait for a bit and retry.
		select {
		case <-timeout:
			panic("timeout waiting for lock file")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Release the lock file when we're done.
	defer func() {
		// Always close file first, Windows won't allow us to remove it
		// otherwise.
		_ = lockFileHandle.Close()
		err := os.Remove(lockFile)
		if err != nil {
			panic(fmt.Errorf("couldn't remove lock file: %w", err))
		}
	}()

	portFile := filepath.Join(os.TempDir(), uniquePortFile)
	port, err := os.ReadFile(portFile)
	if err != nil {
		if !os.IsNotExist(err) {
			panic(fmt.Errorf("error reading port file: %w", err))
		}
		port = []byte(strconv.Itoa(defaultNodePort))
	}

	lastPort, err := strconv.Atoi(string(port))
	if err != nil {
		panic(fmt.Errorf("error parsing port: %w", err))
	}

	// We take the next one.
	lastPort++
	for lastPort < 65535 {
		// If there are no errors while attempting to listen on this
		// port, close the socket and return it as available. While it
		// could be the case that some other process picks up this port
		// between the time the socket is closed, and it's reopened in
		// the harness node, in practice in CI servers this seems much
		// less likely than simply some other process already being
		// bound at the start of the tests.
		addr := fmt.Sprintf(listenerFormat, lastPort)
		l, err := net.Listen("tcp4", addr)
		if err == nil {
			err := l.Close()
			if err == nil {
				err := os.WriteFile(
					portFile,
					[]byte(strconv.Itoa(lastPort)), 0600,
				)
				if err != nil {
					panic(fmt.Errorf("error updating "+
						"port file: %w", err))
				}

				return lastPort
			}
		}
		lastPort++

		// Start from the beginning if we reached the end of the port
		// range. We need to do this because the lock file now is
		// persistent across runs on the same machine during the same
		// boot/uptime cycle. So in order to make this work on
		// developer's machines, we need to reset the port to the
		// default value when we reach the end of the range.
		if lastPort == 65535 {
			lastPort = defaultNodePort
		}
	}

	// No ports available? Must be a mistake.
	panic("no ports available for listening")
}
