package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedGetChanInfoResponse = "{\n    " +
		"\"channel_id\": \"0\",\n    " +
		"\"chan_point\": \"\",\n    " +
		"\"last_update\": 0,\n    " +
		"\"node1_pub\": \"\",\n    " +
		"\"node2_pub\": \"\",\n    " +
		"\"capacity\": \"0\",\n    " +
		"\"node1_policy\": null,\n    " +
		"\"node2_policy\": null\n}\n"
)

func TestGetChanInfo(t *testing.T) {
	TestCommandNoError(t, runGetChanInfo,
		[]string{"234"},
		expectedGetChanInfoRequest(),
		expectedGetChanInfoResponse)
}

func TestGetChanInfo_ChanIdFlag(t *testing.T) {
	TestCommandNoError(t, runGetChanInfo,
		[]string{"--chan_id", "234"},
		expectedGetChanInfoRequest(),
		expectedGetChanInfoResponse)
}

// Bug: Error occurs but then is completely ignored (nothing is printed),
// and then the function proceeds as if there was no error.
func TestGetChanInfo_BadChanId(t *testing.T) {
	TestCommandTextInResponse(t, runGetChanInfo,
		[]string{"BadChanId"},
		expectedGetChanInfoResponse)
}

func TestGetChanInfo_BadChanIdFlag(t *testing.T) {
	TestCommandTextInResponse(t, runGetChanInfo,
		[]string{"--chan_id", "BadChanId"},
		"getchaninfo - get the state of a channel")
}

func TestGetChanInfo_MissingChanId(t *testing.T) {
	TestCommandValidationError(t, runGetChanInfo,
		[]string{},
		ErrMissingChanId)
}

func TestGetChanInfo_RPCError(t *testing.T) {
	TestCommandRPCError(t, runGetChanInfo,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runGetChanInfo(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, getChanInfoCommand, getChanInfo, "getchaninfo", args)
}

func expectedGetChanInfoRequest() *lnrpc.ChanInfoRequest {
	return &lnrpc.ChanInfoRequest{ChanId: 0xea}
}
