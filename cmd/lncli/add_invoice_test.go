package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedAddInvoiceResponse = "{\n\t\"r_hash\": \"7b\",\n\t\"pay_req\": \"PaymentRequest\"\n}\n"

// connectPeer returns the correct output if no errors occurred.
func TestAddInvoice(t *testing.T) {
	TestCommandNoError(t, runAddInvoice,
		[]string{
			PushAmount,
			Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		expectedInvoice(),
		expectedAddInvoiceResponse)
}

func TestAddInvoice_AmtFlag(t *testing.T) {
	TestCommandNoError(t, runAddInvoice,
		[]string{
			Preimage,
			"--amt", PushAmount,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		expectedInvoice(),
		expectedAddInvoiceResponse)
}

func TestAddInvoice_PreimageFlag(t *testing.T) {
	TestCommandNoError(t, runAddInvoice,
		[]string{
			PushAmount,
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		expectedInvoice(),
		expectedAddInvoiceResponse)
}

func TestAddInvoice_AllFlags(t *testing.T) {
	expectedRequest := expectedInvoice()
	expectedRequest.Memo = "Memo"
	expectedRequest.FallbackAddr = "FallbackAddr"
	expectedRequest.Expiry = 987654321

	TestCommandNoError(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt,
			"--memo", "Memo",
			"--fallback_addr", "FallbackAddr",
			"--expiry", "987654321"},
		expectedRequest,
		expectedAddInvoiceResponse)
}

func TestAddInvoice_BadAmt(t *testing.T) {
	TestCommandTextInValidationError(t, runAddInvoice,
		[]string{
			"BadPushAmount",
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		"unable to decode amt argument")
}

func TestAddInvoice_BadAmtFlag(t *testing.T) {
	TestCommandTextInResponse(t, runAddInvoice,
		[]string{
			"--amt", "BadPushAmount",
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		"invalid syntax")
}

func TestAddInvoice_NonHexPreimage(t *testing.T) {
	TestCommandTextInValidationError(t, runAddInvoice,
		[]string{
			"NonHexPreimage",
			"--amt", PushAmount,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		"unable to parse preimage:")
}

func TestAddInvoice_NonHexPreimageFlag(t *testing.T) {
	TestCommandTextInValidationError(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", "NonHexPreimage",
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		"unable to parse preimage:")
}

func TestAddInvoice_NonHexDescriptionHash(t *testing.T) {
	TestCommandTextInValidationError(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", Preimage,
			"--description_hash", "BadDescriptionHash",
			"--receipt", Receipt},
		"unable to parse description_hash:")
}

func TestAddInvoice_NonHexReceipt(t *testing.T) {
	TestCommandTextInValidationError(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", "BadReceipt"},
		"unable to parse receipt:")
}

func TestAddInvoice_BadExpiry(t *testing.T) {
	TestCommandTextInResponse(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt,
			"--expiry", "BadExpiry"},
		"addinvoice - add a new invoice")
}

func TestAddInvoice_RPCError(t *testing.T) {
	TestCommandRPCError(t, runAddInvoice,
		[]string{
			"--amt", PushAmount,
			"--preimage", Preimage,
			"--description_hash", DescriptionHash,
			"--receipt", Receipt},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runAddInvoice(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, addInvoiceCommand, addInvoice, "addinvoice", args)
}

func expectedInvoice() *lnrpc.Invoice {
	return &lnrpc.Invoice{
		Value:           PushAmountInt,
		Receipt:         []uint8{0x78, 0x9a, 0xaa},
		DescriptionHash: []uint8{0x45, 0x6d, 0xef},
		RPreimage:       []uint8{0x12, 0x3a, 0xbc}}
}
