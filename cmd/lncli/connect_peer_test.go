package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedConnectPeerResponse = "{\n\n}\n"

// TestConnectPeer verifies that connectPeer returns the correct output
// if no errors occurred.
func TestConnectPeer(t *testing.T) {
	TestCommandNoError(t, runConnectPeer,
		[]string{GoodAddress},
		expectedConnectPeerRequest(HostWithPort, false),
		expectedConnectPeerResponse)
}

// TestConnectPeer_NoPort verifies that connectPeer doesn't require
// a port in order to successfully connect.
func TestConnectPeer_NoPort(t *testing.T) {
	TestCommandNoError(t, runConnectPeer,
		[]string{GoodAddressWithoutPort},
		expectedConnectPeerRequest(Host, false),
		expectedConnectPeerResponse)
}

// TestConnectPeer_Perm verifies that connectPeer passes "perm" to the RPC.
func TestConnectPeer_Perm(t *testing.T) {
	TestCommandNoError(t, runConnectPeer,
		[]string{"--perm", GoodAddress},
		expectedConnectPeerRequest(HostWithPort, true),
		expectedConnectPeerResponse)
}

// TestConnectPeer_BadAddressFormat verifies that connectPeer returns the
// correct error if an invalid address was specified.
func TestConnectPeer_BadAddressFormat(t *testing.T) {
	TestCommandValidationError(t, runConnectPeer,
		[]string{BadAddress},
		ErrBadAddressFormat)
}

// TestConnectPeer_FailedConnecting verifies that connectPeer bubbles up
// the error if the LightningClient fails to connect.
func TestConnectPeer_FailedConnecting(t *testing.T) {
	TestCommandRPCError(t, runConnectPeer,
		[]string{GoodAddress},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runConnectPeer(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, connectCommand, connectPeer, "connect", args)
}

func expectedConnectPeerRequest(host string, perm bool) *lnrpc.ConnectPeerRequest {
	request := lnrpc.ConnectPeerRequest{
		&lnrpc.LightningAddress{PubKey, host},
		perm}

	return &request
}
