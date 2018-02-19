package main

import (
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

func TestNewAddress_WitnessPubkey(t *testing.T) {
	expectedRequest := lnrpc.NewAddressRequest{
		lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH}
	testErrorlessNewAddress(t, []string{"p2wkh"}, &expectedRequest)
}

func TestNewAddress_NestedPubkey(t *testing.T) {
	expectedRequest := lnrpc.NewAddressRequest{
		lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH}
	testErrorlessNewAddress(t, []string{"np2wkh"}, &expectedRequest)
}

func TestNewAddress_Pubkey(t *testing.T) {
	expectedRequest := lnrpc.NewAddressRequest{
		lnrpc.NewAddressRequest_PUBKEY_HASH}
	testErrorlessNewAddress(t, []string{"p2pkh"}, &expectedRequest)
}

func TestNewAddress_NoAddressType(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testNewAddress(&client, []string{})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "invalid address type"))
}

func TestNewAddress_BadAddressType(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testNewAddress(&client, []string{"BadAddressType"})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "invalid address type"))
}

func TestNewAddress_FailedCreation(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testNewAddress(&client, []string{"p2pkh"})
	require.Error(t, err)
	require.Equal(t, io.EOF, err)
}

func testNewAddress(
	client lnrpc.LightningClient, args []string) (string, error) {

	return TestCommand(
		client, newAddressCommand, newAddress, "newaddress", args)
}

func testErrorlessNewAddress(
	t *testing.T, args []string, expectedRequest *lnrpc.NewAddressRequest) {

	client := lnrpctesting.NewStubLightningClient()
	resp, err := testNewAddress(&client, args)
	require.NoError(t, err)
	require.Equal(t, "{\n    \"address\": \"Address\"\n}\n", resp)
	require.Equal(t, expectedRequest, client.CapturedNewAddressRequest)
}
