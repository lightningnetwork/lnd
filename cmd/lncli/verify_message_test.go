package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedVerifyMessageRequest = lnrpc.VerifyMessageRequest{Msg: []uint8{
		0x4d, 0x65, 0x73, 0x73, 0x61, 0x67, 0x65, 0x20, 0x74, 0x6f, 0x20, 0x56, 0x65, 0x72, 0x69, 0x66, 0x79},
		Signature: "Signature"}
	expectedVerifyMessageResponse = "{\n    \"valid\": false,\n    \"pubkey\": \"\"\n}\n"
)

func TestVerifyMessage(t *testing.T) {
	TestCommandNoError(t, runVerifyMessage,
		[]string{"Message to Verify", "Signature"},
		&expectedVerifyMessageRequest,
		expectedVerifyMessageResponse)
}

func TestVerifyMessage_MsgFlag(t *testing.T) {
	TestCommandNoError(t, runVerifyMessage,
		[]string{"--msg", "Message to Verify", "Signature"},
		&expectedVerifyMessageRequest,
		expectedVerifyMessageResponse)
}

func TestVerifyMessage_SigFlag(t *testing.T) {
	TestCommandNoError(t, runVerifyMessage,
		[]string{"Message to Verify", "--sig", "Signature"},
		&expectedVerifyMessageRequest,
		expectedVerifyMessageResponse)
}

func TestVerifyMessage_NoMsg(t *testing.T) {
	TestCommandValidationError(t, runVerifyMessage,
		[]string{"--sig", "Signature"},
		ErrMissingMessage)
}

func TestVerifyMessage_NoSig(t *testing.T) {
	TestCommandValidationError(t, runVerifyMessage,
		[]string{"--msg", "Message To Verify"},
		ErrMissingSignature)
}

func TestVerifyMessage_RPCError(t *testing.T) {
	TestCommandRPCError(t, runVerifyMessage,
		[]string{"Message to Verify", "Signature"},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runVerifyMessage(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, verifyMessageCommand, verifyMessage, "verifymessage", args)
}
