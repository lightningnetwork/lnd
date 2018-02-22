package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedChannelBalanceResponse = "{\n    \"balance\": \"42\"\n}\n"

func TestChannelBalance(t *testing.T) {
	TestCommandNoError(t, runChannelBalance,
		[]string{},
		&lnrpc.ChannelBalanceRequest{},
		expectedChannelBalanceResponse)
}

func TestChannelBalance_RPCError(t *testing.T) {
	TestCommandRPCError(t, runChannelBalance,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runChannelBalance(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, channelBalanceCommand, channelBalance, "channelbalance", args)
}
