package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedGetNodeInfoResponse = "{\n    " +
		"\"node\": null,\n    " +
		"\"num_channels\": 0,\n    " +
		"\"total_capacity\": \"0\"\n}\n"
)

func TestGetNodeInfo(t *testing.T) {
	TestCommandNoError(t, runGetNodeInfo,
		[]string{PubKey},
		expectedGetNodeInfoRequest(),
		expectedGetNodeInfoResponse)
}

func TestGetNodeInfo_ChanIdFlag(t *testing.T) {
	TestCommandNoError(t, runGetNodeInfo,
		[]string{"--pub_key", PubKey},
		expectedGetNodeInfoRequest(),
		expectedGetNodeInfoResponse)
}

// Bug: Error occurs but then is completely ignored (nothing is printed),
// and then the function proceeds as if there was no error.
func TestGetNodeInfo_BadChanId(t *testing.T) {
	TestCommandTextInResponse(t, runGetNodeInfo,
		[]string{"BadChanId"},
		expectedGetNodeInfoResponse)
}

func TestGetNodeInfo_BadChanIdFlag(t *testing.T) {
	TestCommandTextInResponse(t, runGetNodeInfo,
		[]string{"--pub_key", "BadPubKey"},
		expectedGetNodeInfoResponse)
}

func TestGetNodeInfo_MissingChanId(t *testing.T) {
	TestCommandValidationError(t, runGetNodeInfo,
		[]string{},
		ErrMissingPubKey)
}

func TestGetNodeInfo_RPCError(t *testing.T) {
	TestCommandRPCError(t, runGetNodeInfo,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runGetNodeInfo(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, getNodeInfoCommand, getNodeInfo, "getnodeinfo", args)
}

func expectedGetNodeInfoRequest() *lnrpc.NodeInfoRequest {
	return &lnrpc.NodeInfoRequest{PubKey: PubKey}
}
