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
	c := &Controller{conn: proxy.clientConn, activeServiceID: "fakeID"}

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
	c := &Controller{conn: proxy.clientConn, activeServiceID: "fakeID"}

	// Mock a response with a different serviceID.
	serverResp := "250-onions/current=unmatchedID\n250 OK\n"

	// Let the server mocks a given response.
	_, err := server.Write([]byte(serverResp))
	require.NoError(t, err, "server failed to write")

	// Check the error returned from GetServiceInfo is expected.
	require.ErrorIs(t, c.CheckOnionService(), ErrServiceIDMismatch)
}

func TestCheckOnionServiceSucceedOnMultipleServices(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign a fake service ID to the controller.
	c := &Controller{conn: proxy.clientConn, activeServiceID: "fakeID"}

	// Mock a response with a different serviceID.
	serverResp := "250-onions/current=service1,fakeID,service2\n250 OK\n"

	// Let the server mocks a given response.
	_, err := server.Write([]byte(serverResp))
	require.NoError(t, err, "server failed to write")

	// No error is expected, the controller's ID is contained within the
	// list of active services.
	require.NoError(t, c.CheckOnionService())
}

func TestCheckOnionServiceFailOnClosedConnection(t *testing.T) {
	t.Parallel()

	// Create mock server and client connection.
	proxy := createTestProxy(t)
	t.Cleanup(proxy.cleanUp)
	server := proxy.serverConn

	// Assign a fake service ID to the controller.
	c := &Controller{conn: proxy.clientConn, activeServiceID: "fakeID"}

	// Close the connection from the server side.
	require.NoError(t, server.Close(), "server failed to close conn")

	// Check the error returned from GetServiceInfo is expected.
	err := c.CheckOnionService()
	eof := errors.Is(err, io.EOF)
	reset := errors.Is(err, syscall.ECONNRESET)
	require.Truef(t, eof || reset,
		"must of EOF or RESET error, instead got: %v", err)
}
