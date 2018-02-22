package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedLookupInvoiceResponse = "{\n    " +
		"\"memo\": \"\",\n    " +
		"\"receipt\": null,\n    " +
		"\"r_preimage\": null,\n    " +
		"\"r_hash\": null,\n    " +
		"\"value\": \"0\",\n    " +
		"\"settled\": false,\n    " +
		"\"creation_date\": \"0\",\n    " +
		"\"settle_date\": \"0\",\n    " +
		"\"payment_request\": \"\",\n    " +
		"\"description_hash\": null,\n    " +
		"\"expiry\": \"0\",\n    " +
		"\"fallback_addr\": \"\",\n    " +
		"\"cltv_expiry\": \"0\"\n}\n"

	rhash = "123abc"
)

func TestLookupInvoice(t *testing.T) {
	TestCommandNoError(t, runLookupInvoice,
		[]string{rhash},
		expectedLookupInvoiceRequest(),
		expectedLookupInvoiceResponse)
}

func TestLookupInvoice_RhashFlag(t *testing.T) {
	TestCommandNoError(t, runLookupInvoice,
		[]string{"--rhash", rhash},
		expectedLookupInvoiceRequest(),
		expectedLookupInvoiceResponse)
}

func TestLookupInvoice_BadRhash(t *testing.T) {
	TestCommandTextInValidationError(t, runLookupInvoice,
		[]string{"BadRhash"},
		"unable to decode rhash argument")
}

func TestLookupInvoice_BadRhashFlag(t *testing.T) {
	TestCommandTextInValidationError(t, runLookupInvoice,
		[]string{"BadRhash"},
		"unable to decode rhash argument")
}

func TestLookupInvoice_MissingRhash(t *testing.T) {
	TestCommandValidationError(t, runLookupInvoice,
		[]string{},
		ErrMissingRhash)
}

func TestLookupInvoice_RPCError(t *testing.T) {
	TestCommandRPCError(t, runLookupInvoice,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runLookupInvoice(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, lookupInvoiceCommand, lookupInvoice, "lookupinvoice", args)
}

func expectedLookupInvoiceRequest() *lnrpc.PaymentHash {
	return &lnrpc.PaymentHash{RHash: []uint8{0x12, 0x3a, 0xbc}}
}
