package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedSignMessageRequest = lnrpc.SignMessageRequest{Msg: []uint8{
		0x4d, 0x65, 0x73, 0x73, 0x61, 0x67, 0x65, 0x20, 0x74, 0x6f, 0x20, 0x53, 0x69, 0x67, 0x6e}}
	expectedSignMessageResponse = "{\n    \"signature\": \"Signature\"\n}\n"
)

func TestSignMessage(t *testing.T) {
	TestCommandNoError(t, runSignMessage,
		[]string{"Message to Sign"},
		&expectedSignMessageRequest,
		expectedSignMessageResponse)
}

func TestSignMessage_MsgFlag(t *testing.T) {
	TestCommandNoError(t, runSignMessage,
		[]string{"--msg", "Message to Sign"},
		&expectedSignMessageRequest,
		expectedSignMessageResponse)
}

func TestSignMessage_NoMsg(t *testing.T) {
	TestCommandValidationError(t, runSignMessage,
		[]string{},
		ErrMissingMsg)
}

func TestSignMessage_RPCError(t *testing.T) {
	TestCommandRPCError(t, runSignMessage,
		[]string{"Message to Sign"},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runSignMessage(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, signMessageCommand, signMessage, "signmessage", args)
}
