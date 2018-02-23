package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedListChainTxnsResponse = "{\n    \"transactions\": [\n        {\n            " +
		"\"tx_hash\": \"TxHash\",\n            " +
		"\"amount\": \"42\",\n            " +
		"\"num_confirmations\": 0,\n            " +
		"\"block_hash\": \"\",\n            " +
		"\"block_height\": 0,\n            " +
		"\"time_stamp\": \"0\",\n            " +
		"\"total_fees\": \"0\",\n            " +
		"\"dest_addresses\": [\n            ]\n        }\n    ]\n}\n"
)

func TestListChainTxns(t *testing.T) {
	TestCommandNoError(t, runListChainTxns,
		[]string{},
		&lnrpc.GetTransactionsRequest{},
		expectedListChainTxnsResponse)
}

func TestListChainTxns_RPCError(t *testing.T) {
	TestCommandRPCError(t, runListChainTxns,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runListChainTxns(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, listChainTxnsCommand, listChainTxns, "listchaintxns", args)
}
