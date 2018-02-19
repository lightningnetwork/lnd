package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

// connectPeer returns the correct output if no errors occurred.
func TestConnectPeer(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(&client, []string{GoodAddress})
	require.NoError(t, err)
	require.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")
}

// connectPeer doesn't require a port in order to successfully connect.
func TestConnectPeer_NoPort(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(&client, []string{GoodAddressWithoutPort})
	require.NoError(t, err)
	require.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")
}

// connectPeer returns the correct error if an invalid address was specified.
func TestConnectPeer_BadAddressFormat(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testConnectPeer(&client, []string{BadAddress})
	require.Error(t, err)
	require.Equal(t, ErrBadAddressFormat, err, "Incorrect error returned")
}

// connectPeer bubbles up the error if the LightningClient fails to connect.
func TestConnectPeer_FailedConnecting(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testConnectPeer(&client, []string{GoodAddress})
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// connectPeer returns a friendly error message upon EOF errors.
func TestConnectPeer_FailedConnectingWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testConnectPeer(&client, []string{GoodAddress})
	require.Error(t, err)
	require.Equal(t, io.EOF, err, "Incorrect error returned.")
}

func testConnectPeer(
	client lnrpc.LightningClient, args []string) (string, error) {

	return TestCommand(
		client, connectCommand, connectPeer, "connect", args)
}
