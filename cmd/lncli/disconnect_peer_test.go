package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

// disconnectPeer returns the correct output if no errors occur.
func TestDisconnectPeer(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testDisconnectPeer(&client, []string{PubKey})
	require.NoError(t, err)
	require.Equal(t, "{\n\n}\n", resp,
		"Incorrect JSON response from disconnectPeer.")

	expectedRequest := lnrpc.DisconnectPeerRequest{PubKey}
	require.Equal(t, &expectedRequest, client.CapturedDisconnectPeerRequest)
}

// disconnectPeer can take the pubkey as a flag instead of arg.
func TestDisconnectPeer_NodeKeyFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testDisconnectPeer(&client, []string{"--node_key", PubKey})
	require.NoError(t, err)
	require.Equal(t, "{\n\n}\n", resp,
		"Incorrect JSON response from disconnectPeer.")

	expectedRequest := lnrpc.DisconnectPeerRequest{PubKey}
	require.Equal(t, &expectedRequest, client.CapturedDisconnectPeerRequest)
}

// disconnectPeer returns the correct error if no pubkey was specified
func TestDisconnectPeer_NoPubkey(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testDisconnectPeer(&client, []string{})
	require.Error(t, err)
	require.Equal(t, ErrMissingPubKey, err, "Incorrect error returned.")
}

// disconnectPeer bubbles up the error if the LightningClient fails to connect.
func TestDisconnectPeer_FailedDisconnecting(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testDisconnectPeer(&client, []string{PubKey})
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// disconnectPeer returns a friendly error message upon EOF errors.
func TestDisconnectPeer_FailedDisconnectingWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testDisconnectPeer(&client, []string{PubKey})
	require.Error(t, err)
	require.Equal(t, io.EOF, err, "Incorrect error returned.")
}

func testDisconnectPeer(client lnrpc.LightningClient, args []string) (string, error) {
	return TestCommand(
		client, disconnectCommand, disconnectPeer, "disconnect", args)
}
