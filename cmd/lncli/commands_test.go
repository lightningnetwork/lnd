package main

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/assert"
	"github.com/urfave/cli"
)

var GoodAddress = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org:1234"
var GoodAddressWithoutPort = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org"
var BadAddress = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"

type StringWriter struct {
	outputs []string
}

func (w *StringWriter) Write(p []byte) (n int, err error) {
	w.outputs = append(w.outputs, string(p))
	return len(p), nil
}

func (w *StringWriter) Join() string {
	return strings.Join(w.outputs, "\n")
}

func testConnectPeer(t *testing.T, client lnrpc.LightningClient, address string) (string, error) {
	set := flag.NewFlagSet("test", 0)

	context := cli.NewContext(nil, set, nil)
	set.Parse([]string{address})

	writer := StringWriter{}
	err := connectPeer(context, client, &writer)
	if err == nil {
		return writer.Join(), nil
	} else {
		return "", err
	}
}

// connectPeer returns the correct output if no errors occurred.
func TestConnectPeer(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(t, &client, GoodAddress)
	assert.NoError(t, err)
	assert.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")

}

// connectPeer doesn't require a port in order to successfully connect.
func TestConnectPeerNoPort(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testConnectPeer(t, &client, GoodAddressWithoutPort)
	assert.NoError(t, err)
	assert.Equal(t, "{\n    \"peer_id\": 0\n}\n", resp,
		"Incorrect JSON response from connectPeer.")
}

// connectPeer returns the correct error if an invalid address was specified.
func TestConnectPeerBadAddressFormat(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testConnectPeer(t, &client, BadAddress)
	assert.Error(t, err)
	assert.Equal(t, ErrBadAddressFormat, err, "Incorrect error returned")
}

// connectPeer bubbles up the error if the LightningClient fails to connect.
func TestConnectPeerFailedConnecting(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testConnectPeer(t, &client, GoodAddress)
	assert.Error(t, err)
	assert.Equal(t, io.ErrClosedPipe, err, "Incorrect error returned.")
}

// connectPeer bubbles up the EOF error if the LightningClient fails to connect.
// TODO(merehap): add a better error message for this case and any similar cases.
func TestConnectPeerFailedConnectingWithEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testConnectPeer(t, &client, GoodAddress)
	assert.Error(t, err)
	assert.Equal(t, io.EOF, err, "Incorrect error returned.")
}
