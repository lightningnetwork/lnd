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

func testDisconnectPeer(
	t *testing.T, client lnrpc.LightningClient, args []string) (string, error) {

	set := flag.NewFlagSet("test", 0)
	context := cli.NewContext(nil, set, nil)
	set.Parse(args)

	writer := StringWriter{}
	err := disconnectPeer(context, client, &writer)
	if err != nil {
		return "", err
	}

	return writer.Join(), nil
}

// disconnectPeer returns the correct output if no errors occur.
func TestDisconnectPeer(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testDisconnectPeer(t, &client, []string{PubKey})
	require.NoError(t, err)
	require.Equal(t, "{\n\n}\n", resp,
		"Incorrect JSON response from disconnectPeer.")
}

// disconnectPeer can take the pubkey as a flag instead of arg.
func TestDisconnectPeer_NodeKeyFlag(t *testing.T) {

	set := flag.NewFlagSet("test", 0)
	set.String("node_key", "default value", "doc")

	context := cli.NewContext(nil, set, nil)
	set.Parse([]string{"--node_key", PubKey})

	client := lnrpctesting.NewStubLightningClient()
	writer := StringWriter{}
	err := disconnectPeer(context, &client, &writer)
	require.NoError(t, err)
	require.Equal(t, "{\n\n}\n", writer.Join(),
		"Incorrect JSON response from disconnectPeer.")
}

// disconnectPeer returns the correct error if no pubkey was specified
func TestDisconnectPeer_NoPubkey(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testDisconnectPeer(t, &client, []string{})
	require.Error(t, err)
	require.Equal(t, ErrMissingPubKey, err, "Incorrect error returned.")
}

// disconnectPeer bubbles up the error if the LightningClient fails to connect.
func TestDisconnectPeer_FailedDisconnecting(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testDisconnectPeer(t, &client, []string{PubKey})
	require.Error(t, err)
	require.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// disconnectPeer returns a friendly error message upon EOF errors.
func TestDisconnectPeer_FailedDisconnectingWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testDisconnectPeer(t, &client, []string{PubKey})
	require.Error(t, err)
	require.Equal(t, io.EOF, err, "Incorrect error returned.")
}
