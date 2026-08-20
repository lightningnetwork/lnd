package tor

import (
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckOnionServiceFailOnServiceNotCreated(t *testing.T) {
	t.Parallel()

	// Create a dummy tor controller.
	c := &Controller{}

	// Check that CheckOnionService returns an error when the service
	// hasn't been created.
	require.Equal(t, ErrServiceNotCreated, c.CheckOnionService())
}

func TestCheckOnionServiceSucceed(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign a fake service ID to the controller.
	c := &Controller{
		conn: proxy.clientConn,
		activeServiceIDs: map[string]struct{}{
			"fakeID": {},
		},
	}

	// Test a successful response.
	serverResp := "250-onions/current=fakeID\n250 OK\n"

	// Let the server mocks a given response.
	_, err := server.Write([]byte(serverResp))
	require.NoError(t, err, "server failed to write")

	// For a successful response, we expect no error.
	require.NoError(t, c.CheckOnionService())
}

func TestCheckOnionServiceFailOnServiceIDNotMatch(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign a fake service ID to the controller.
	c := &Controller{
		conn: proxy.clientConn,
		activeServiceIDs: map[string]struct{}{
			"fakeID": {},
		},
	}

	// Mock a response with a different serviceID.
	serverResp := "250-onions/current=unmatchedID\n250 OK\n"

	// Let the server mocks a given response.
	_, err := server.Write([]byte(serverResp))
	require.NoError(t, err, "server failed to write")

	// Check the error returned from GetServiceInfo is expected.
	require.ErrorIs(t, c.CheckOnionService(), ErrServiceIDMismatch)
}

func TestCheckOnionServiceExactMultipleServices(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign two fake service IDs to the controller.
	c := &Controller{
		conn: proxy.clientConn,
		registrations: []onionServiceRegistration{
			{serviceID: "service1"},
			{serviceID: "service2"},
		},
		activeServiceIDs: map[string]struct{}{
			"service1": {},
			"service2": {},
		},
	}

	// Reordering the exact service set is accepted.
	serverResp := "250-onions/current=service2,service1\n250 OK\n"

	// Let the server mocks a given response.
	_, err := server.Write([]byte(serverResp))
	require.NoError(t, err, "server failed to write")

	// No error is expected because every tracked service is present and
	// there are no unexpected services.
	require.NoError(t, c.CheckOnionService())
}

func TestCheckOnionServiceRejectsInexactSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response string
	}{
		{
			name:     "missing service",
			response: "service1",
		},
		{
			name:     "partial service name",
			response: "service1,service",
		},
		{
			name:     "unexpected service",
			response: "service1,service2,service3",
		},
		{
			name:     "duplicate service",
			response: "service1,service1",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			proxy := createTestProxy(t)
			t.Cleanup(proxy.cleanUp)
			c := &Controller{
				conn: proxy.clientConn,
				activeServiceIDs: map[string]struct{}{
					"service1": {},
					"service2": {},
				},
			}

			_, err := proxy.serverConn.Write([]byte(
				"250-onions/current=" + test.response +
					"\n250 OK\n",
			))
			require.NoError(t, err)
			require.ErrorIs(
				t, c.CheckOnionService(), ErrServiceIDMismatch,
			)
		})
	}
}

func TestCheckOnionServiceFailOnClosedConnection(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign a fake service ID to the controller.
	c := &Controller{
		conn: proxy.clientConn,
		activeServiceIDs: map[string]struct{}{
			"fakeID": {},
		},
	}

	// Close the connection from the server side.
	require.NoError(t, server.Close(), "server failed to close conn")

	// Check the error returned from GetServiceInfo is expected.
	err := c.CheckOnionService()
	eof := errors.Is(err, io.EOF)
	reset := errors.Is(err, syscall.ECONNRESET)
	require.Truef(t, eof || reset,
		"must of EOF or RESET error, instead got: %v", err)
}
