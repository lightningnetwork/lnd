package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedListPeersResponseText = "{\n    \"peers\": [\n        {\n            " +
	"\"pub_key\": \"PubKey\",\n            " +
	"\"address\": \"Address\",\n            " +
	"\"bytes_sent\": \"78\",\n            " +
	"\"bytes_recv\": \"89\",\n            " +
	"\"sat_sent\": \"123\",\n            " +
	"\"sat_recv\": \"456\",\n            " +
	"\"inbound\": true,\n            " +
	"\"ping_time\": \"7890\"\n        }\n    ]\n}\n"

func TestListPeers(t *testing.T) {
	TestCommandNoError(t, runListPeers,
		[]string{},
		&lnrpc.ListPeersRequest{},
		expectedListPeersResponseText)
}

func TestListPeers_RPCError(t *testing.T) {
	TestCommandRPCError(t, runListPeers,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runListPeers(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, listPeersCommand, listPeers, "listpeers", args)
}
