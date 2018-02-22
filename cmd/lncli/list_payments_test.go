package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedListPaymentsResponse = "{\n    \"payments\": [\n        {\n            " +
		"\"payment_hash\": \"PaymentHash\",\n            " +
		"\"value\": \"765\",\n            " +
		"\"creation_date\": \"0\",\n            " +
		"\"path\": [\n            ],\n            " +
		"\"fee\": \"0\",\n            " +
		"\"payment_preimage\": \"\"\n        }\n    ]\n}\n"
)

func TestListPayments(t *testing.T) {
	TestCommandNoError(t, runListPayments,
		[]string{rhash},
		expectedListPaymentsRequest(),
		expectedListPaymentsResponse)
}

func TestListPayments_RPCError(t *testing.T) {
	TestCommandRPCError(t, runListPayments,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runListPayments(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, listPaymentsCommand, listPayments, "listpayments", args)
}

func expectedListPaymentsRequest() *lnrpc.ListPaymentsRequest {
	return &lnrpc.ListPaymentsRequest{}
}
