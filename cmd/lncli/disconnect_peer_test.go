package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedDisconnectPeerResponse = "{\n\n}\n"

// TestDisconnectPeer verifies that disconnectPeer returns
// the correct output if no errors occur.
func TestDisconnectPeer(t *testing.T) {
	TestCommandNoError(t, runDisconnectPeer,
		[]string{PubKey},
		&lnrpc.DisconnectPeerRequest{PubKey},
		expectedDisconnectPeerResponse)
}

// TestDisconnectPeer_NodeKeyFlag verifies that disconnectPeer can take
// the pubkey as a flag instead of arg.
func TestDisconnectPeer_NodeKeyFlag(t *testing.T) {
	TestCommandNoError(t, runDisconnectPeer,
		[]string{"--node_key", PubKey},
		&lnrpc.DisconnectPeerRequest{PubKey},
		expectedDisconnectPeerResponse)
}

// TestDisconnectPeer_NoPubkey verifies that disconnectPeer returns the
// correct error if no pubkey was specified
func TestDisconnectPeer_NoPubkey(t *testing.T) {
	TestCommandValidationError(t, runDisconnectPeer,
		[]string{},
		ErrMissingPubKey)
}

// TestDisconnectPeer_FailedDisconnecting verifies that disconnectPeer
// bubbles up the error if the LightningClient fails to connect.
func TestDisconnectPeer_FailedDisconnecting(t *testing.T) {
	TestCommandRPCError(t, runDisconnectPeer,
		[]string{PubKey},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runDisconnectPeer(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, disconnectCommand, disconnectPeer, "disconnect", args)
}
