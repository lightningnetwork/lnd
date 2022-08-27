package tor

import (
	"net"
	"net/textproto"
	"testing"

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
	// Set up the proxy to listen on given port.
	//
	// NOTE: we use a port 0 here to indicate we want a free port selected
	// by the system.
	proxy, err := net.Listen("tcp", ":0")
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

	// Accept the connection inside a goroutine. We will also write some
	// data so that the reconnection can succeed. We will mock three writes
	// and two reads inside our proxy server,
	//   - write protocol info
	//   - read auth info
	//   - write auth challenge
	//   - read auth challenge
	//   - write OK
	go func() {
		// Accept the new connection.
		server, err := proxy.server.Accept()
		require.NoError(t, err, "failed to accept")

		// Write the protocol info.
		resp := "250-PROTOCOLINFO 1\n" +
			"250-AUTH METHODS=NULL\n" +
			"250 OK\n"
		_, err = server.Write([]byte(resp))
		require.NoErrorf(t, err, "failed to write protocol info")

		// Read the auth info from the client.
		buf := make([]byte, 65535)
		_, err = server.Read(buf)
		require.NoError(t, err)

		// Write the auth challenge.
		resp = "250 AUTHCHALLENGE SERVERHASH=fake\n"
		_, err = server.Write([]byte(resp))
		require.NoErrorf(t, err, "failed to write auth challenge")

		// Read the auth challenge resp from the client.
		_, err = server.Read(buf)
		require.NoError(t, err)

		// Write OK resp.
		resp = "250 OK\n"
		_, err = server.Write([]byte(resp))
		require.NoErrorf(t, err, "failed to write response auth")
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
