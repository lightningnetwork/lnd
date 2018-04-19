package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedWalletBalanceResponse = "{\n    \"total_balance\": \"100\",\n    " +
	"\"confirmed_balance\": \"70\",\n    " +
	"\"unconfirmed_balance\": \"30\"\n}\n"

func TestWalletBalance(t *testing.T) {
	TestCommandNoError(t, runWalletBalance,
		[]string{},
		&lnrpc.WalletBalanceRequest{},
		expectedWalletBalanceResponse)
}

func TestWalletBalance_RPCError(t *testing.T) {
	TestCommandRPCError(t, runWalletBalance,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runWalletBalance(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, walletBalanceCommand, walletBalance, "walletbalance", args)
}
