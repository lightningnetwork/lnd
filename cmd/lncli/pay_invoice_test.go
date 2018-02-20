package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

// Verify that that PayReq is passed to Stream.SendMsg() and that the correct
// SendPaymentResponse is returned.
func TestPayInvoice(t *testing.T) {
	testErrorlessPayInvoice(t,
		[]string{PayReq},
		&lnrpc.SendRequest{PaymentRequest: PayReq})
}

// PayReq can be passed as a flag.
func TestPayInvoice_PayReqFlag(t *testing.T) {
	testErrorlessPayInvoice(t,
		[]string{"--pay_req", PayReq},
		&lnrpc.SendRequest{PaymentRequest: PayReq})
}

// A PayReq can include an amount.
func TestPayInvoice_Amt(t *testing.T) {
	testErrorlessPayInvoice(t,
		[]string{"--amt", "12000", PayReq},
		&lnrpc.SendRequest{PaymentRequest: PayReq, Amt: 12000})
}

// PayReq is required.
func TestPayInvoice_NoPayReq(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := runPayInvoice(&client, []string{})
	require.Equal(t, ErrMissingPayReq, err)
}

// Errors on initiating a payment should be propagated up.
func TestPayInvoice_SendPaymentError(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := runPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// Errors on sending a payment should be propagated up.
func TestPayInvoice_StreamSendError(t *testing.T) {
	stream := NewSendPaymentStream(io.ErrClosedPipe, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// Errors on receiving confirmation of a payment should be propagated up.
func TestPayInvoice_StreamRecvError(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.ErrClosedPipe)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

func runPayInvoice(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(client, payInvoiceCommand, payInvoice, "payinvoice", args)
}

// Test that the payment stream receives the correct input and returns the correct output.
// TestCommandNoError can't be used for this purpose since it checks the response
// of the RPC call, not the stream call.
func testErrorlessPayInvoice(
	t *testing.T, args []string, expectedSendRequest *lnrpc.SendRequest) {

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := runPayInvoice(&client, args)

	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
	require.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
}
