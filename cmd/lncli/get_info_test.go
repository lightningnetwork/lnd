package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedGetInfoResponse = "{\n    \"identity_pubkey\": \"Pubkey\",\n    " +
	"\"alias\": \"Alias\",\n    " +
	"\"num_pending_channels\": 55,\n    " +
	"\"num_active_channels\": 21,\n    " +
	"\"num_peers\": 77,\n    " +
	"\"block_height\": 123456,\n    " +
	"\"block_hash\": \"BlockHash\",\n    " +
	"\"synced_to_chain\": true,\n    " +
	"\"testnet\": true,\n    " +
	"\"chains\": [\n        \"Bitcoin\",\n        \"Litecoin\"\n    ],\n    " +
	"\"uris\": [\n        \"URI0\",\n        \"URI1\"\n    ]\n}\n"

func TestGetInfo(t *testing.T) {
	TestCommandNoError(t, runGetInfo,
		[]string{},
		&lnrpc.GetInfoRequest{},
		expectedGetInfoResponse)
}

func TestGetInfo_RPCError(t *testing.T) {
	TestCommandRPCError(t, runGetInfo,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runGetInfo(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, getInfoCommand, getInfo, "getinfo", args)
}
