package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedDecodePayReqResponse = "{\n    " +
		"\"destination\": \"\",\n    " +
		"\"payment_hash\": \"\",\n    " +
		"\"num_satoshis\": \"0\",\n    " +
		"\"timestamp\": \"0\",\n    " +
		"\"expiry\": \"0\",\n    " +
		"\"description\": \"\",\n    " +
		"\"description_hash\": \"\",\n    " +
		"\"fallback_addr\": \"\",\n    " +
		"\"cltv_expiry\": \"0\"\n}\n"
)

func TestDecodePayReq(t *testing.T) {
	TestCommandNoError(t, runDecodePayReq,
		[]string{"PayReqString"},
		&lnrpc.PayReqString{PayReq: "PayReqString"},
		expectedDecodePayReqResponse)
}

func TestDecodePayReq_PayReqFlag(t *testing.T) {
	TestCommandNoError(t, runDecodePayReq,
		[]string{"--pay_req", "PayReqString"},
		&lnrpc.PayReqString{PayReq: "PayReqString"},
		expectedDecodePayReqResponse)
}

func TestDecodePayReq_NoPayReq(t *testing.T) {
	TestCommandValidationError(t, runDecodePayReq,
		[]string{},
		ErrMissingPayReq)
}

func TestDecodePayReq_RPCError(t *testing.T) {
	TestCommandRPCError(t, runDecodePayReq,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runDecodePayReq(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, decodePayReqCommand, decodePayReq, "decodepayreq", args)
}
