package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedNewAddressResponse = "{\n    \"address\": \"Address\"\n}\n"

func TestNewAddress_WitnessPubkey(t *testing.T) {
	TestCommandNoError(t, runNewAddress,
		[]string{"p2wkh"},
		&lnrpc.NewAddressRequest{lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH},
		expectedNewAddressResponse)
}

func TestNewAddress_NestedPubkey(t *testing.T) {
	TestCommandNoError(t, runNewAddress,
		[]string{"np2wkh"},
		&lnrpc.NewAddressRequest{lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH},
		expectedNewAddressResponse)
}

func TestNewAddress_Pubkey(t *testing.T) {
	TestCommandNoError(t, runNewAddress,
		[]string{"p2pkh"},
		&lnrpc.NewAddressRequest{lnrpc.NewAddressRequest_PUBKEY_HASH},
		expectedNewAddressResponse)
}

func TestNewAddress_NoAddressType(t *testing.T) {
	TestCommandTextInValidationError(t, runNewAddress,
		[]string{},
		"invalid address type")
}

func TestNewAddress_BadAddressType(t *testing.T) {
	TestCommandTextInValidationError(t, runNewAddress,
		[]string{"BadAddressType"},
		"invalid address type")
}

func TestNewAddress_FailedCreation(t *testing.T) {
	TestCommandRPCError(t, runNewAddress,
		[]string{"p2pkh"},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runNewAddress(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, newAddressCommand, newAddress, "newaddress", args)
}
