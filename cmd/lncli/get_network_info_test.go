package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedGetNetworkInfoResponse = "{\n    " +
		"\"graph_diameter\": 0,\n    " +
		"\"avg_out_degree\": 0,\n    " +
		"\"max_out_degree\": 0,\n    " +
		"\"num_nodes\": 0,\n    " +
		"\"num_channels\": 0,\n    " +
		"\"total_network_capacity\": \"0\",\n    " +
		"\"avg_channel_size\": 0,\n    " +
		"\"min_channel_size\": \"0\",\n    " +
		"\"max_channel_size\": \"0\"\n}\n"
)

func TestGetNetworkInfo(t *testing.T) {
	TestCommandNoError(t, runGetNetworkInfo,
		[]string{},
		expectedGetNetworkInfoRequest(),
		expectedGetNetworkInfoResponse)
}

func TestGetNetworkInfo_RPCError(t *testing.T) {
	TestCommandRPCError(t, runGetNetworkInfo,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runGetNetworkInfo(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, getNetworkInfoCommand, getNetworkInfo, "getnetworkinfo", args)
}

func expectedGetNetworkInfoRequest() *lnrpc.NetworkInfoRequest {
	return &lnrpc.NetworkInfoRequest{}
}
