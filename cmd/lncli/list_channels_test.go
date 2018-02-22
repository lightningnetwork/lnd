package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedListChannelsResponse = "{\n    \"channels\": [\n        {\n            " +
	"\"active\": true,\n            " +
	"\"remote_pubkey\": \"RemotePubkey\",\n            " +
	"\"channel_point\": \"\",\n            " +
	"\"chan_id\": \"0\",\n            " +
	"\"capacity\": \"0\",\n            " +
	"\"local_balance\": \"1234\",\n            " +
	"\"remote_balance\": \"5678\",\n            " +
	"\"commit_fee\": \"0\",\n            " +
	"\"commit_weight\": \"0\",\n            " +
	"\"fee_per_kw\": \"0\",\n            " +
	"\"unsettled_balance\": \"0\",\n            " +
	"\"total_satoshis_sent\": \"0\",\n            " +
	"\"total_satoshis_received\": \"0\",\n            " +
	"\"num_updates\": \"0\",\n            " +
	"\"pending_htlcs\": [\n            ],\n            " +
	"\"csv_delay\": 0\n        }\n    ]\n}\n"

func TestListChannels(t *testing.T) {
	TestCommandNoError(t, runListChannels,
		[]string{},
		&lnrpc.ListChannelsRequest{},
		expectedListChannelsResponse)
}

func TestListChannels_RPCError(t *testing.T) {
	TestCommandRPCError(t, runListChannels,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runListChannels(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, listChannelsCommand, listChannels, "listchannels", args)
}
