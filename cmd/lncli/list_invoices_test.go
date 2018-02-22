package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedListInvoicesResponse = "{\n    \"invoices\": [\n        {\n            " +
		"\"memo\": \"\",\n            " +
		"\"receipt\": \"eJqq\",\n            " +
		"\"r_preimage\": \"Ejq8\",\n            " +
		"\"r_hash\": null,\n            " +
		"\"value\": \"5000\",\n            " +
		"\"settled\": false,\n            " +
		"\"creation_date\": \"0\",\n            " +
		"\"settle_date\": \"0\",\n            " +
		"\"payment_request\": \"\",\n            " +
		"\"description_hash\": \"RW3v\",\n            " +
		"\"expiry\": \"0\",\n            " +
		"\"fallback_addr\": \"\",\n            " +
		"\"cltv_expiry\": \"0\"\n        }\n    ]\n}\n"
)

func TestListInvoices(t *testing.T) {
	TestCommandNoError(t, runListInvoices,
		[]string{},
		&lnrpc.ListInvoiceRequest{PendingOnly: false},
		expectedListInvoicesResponse)
}

func TestListInvoices_PendingOnly(t *testing.T) {
	TestCommandNoError(t, runListInvoices,
		[]string{"--pending_only"},
		&lnrpc.ListInvoiceRequest{PendingOnly: true},
		expectedListInvoicesResponse)
}

func TestListInvoices_RPCError(t *testing.T) {
	TestCommandRPCError(t, runListInvoices,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runListInvoices(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, listInvoicesCommand, listInvoices, "listinvoices", args)
}
