package main

import (
	"flag"
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

func testConnectPeer(
	t *testing.T, client lnrpc.LightningClient, address string) (string, error) {

	set := flag.NewFlagSet("test", 0)
	context := cli.NewContext(nil, set, nil)
	set.Parse([]string{address})

	writer := StringWriter{}
	err := connectPeer(context, client, &writer)
	if err != nil {
		return "", err
	}

	return writer.Join(), nil
}

// connectPeer returns the correct output if no errors occurred.
func TestConnectPeer(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(t, &client, GoodAddress)
	require.NoError(t, err)
	require.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")
}

// connectPeer doesn't require a port in order to successfully connect.
func TestConnectPeer_NoPort(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(t, &client, GoodAddressWithoutPort)
	require.NoError(t, err)
	require.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")
}

// connectPeer returns the correct error if an invalid address was specified.
func TestConnectPeer_BadAddressFormat(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testConnectPeer(t, &client, BadAddress)
	require.Error(t, err)
	require.Equal(t, ErrBadAddressFormat, err, "Incorrect error returned")
}

// connectPeer bubbles up the error if the LightningClient fails to connect.
func TestConnectPeer_FailedConnecting(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testConnectPeer(t, &client, GoodAddress)
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// connectPeer returns a friendly error message upon EOF errors.
func TestConnectPeer_FailedConnectingWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testConnectPeer(t, &client, GoodAddress)
	require.Error(t, err)
	require.Equal(t, io.EOF, err, "Incorrect error returned.")
}
