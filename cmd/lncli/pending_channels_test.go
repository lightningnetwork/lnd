package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedPendingChannelsResponse = "{\n    " +
	"\"total_limbo_balance\": \"0\",\n    " +
	"\"pending_open_channels\": [\n    ],\n    " +
	"\"pending_closing_channels\": [\n    ],\n    " +
	"\"pending_force_closing_channels\": [\n    ]\n}\n"

func TestPendingChannels(t *testing.T) {
	TestCommandNoError(t, runPendingChannels,
		[]string{},
		&lnrpc.PendingChannelsRequest{},
		expectedPendingChannelsResponse)
}

func TestPendingChannels_RPCError(t *testing.T) {
	TestCommandRPCError(t, runPendingChannels,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runPendingChannels(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, pendingChannelsCommand, pendingChannels, "pendingchannels", args)
}
