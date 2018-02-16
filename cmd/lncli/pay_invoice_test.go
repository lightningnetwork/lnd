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
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{PayReq})

	require.Equal(t, &expectedSendRequest, stream.CapturedSendRequest)
	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
}

// PayReq can be passed as a flag.
func TestPayInvoice_PayReqFlag(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{"--pay_req", PayReq})

	require.Equal(t, &expectedSendRequest, stream.CapturedSendRequest)
	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
}

// A PayReq can include an amount.
func TestPayInvoice_Amt(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq
	expectedSendRequest.Amt = 12000

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{"--amt", "12000", PayReq})

	require.Equal(t, &expectedSendRequest, stream.CapturedSendRequest)
	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
}

// PayReq is required.
func TestPayInvoice_NoPayReq(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testPayInvoice(&client, []string{})
	require.Equal(t, ErrMissingPayReq, err)
}

// Errors on initiating a payment should be propagated up.
func TestPayInvoice_SendPaymentError(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on initiating a payment are currently propagated up.
func TestPayInvoice_SendPaymentEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.EOF, err)
}

// Errors on sending a payment should be propagated up.
func TestPayInvoice_StreamSendError(t *testing.T) {
	stream := NewSendPaymentStream(io.ErrClosedPipe, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on sending a payment are currently propagated up.
func TestPayInvoice_StreamSendEOF(t *testing.T) {
	stream := NewSendPaymentStream(io.EOF, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.EOF, err)
}

// Errors on receiving confirmation of a payment should be propagated up.
func TestPayInvoice_StreamRecvError(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.ErrClosedPipe)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on receiving confirmation of a payment are currently propagated up.
func TestPayInvoice_StreamRecvEOF(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.EOF)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	require.Equal(t, io.EOF, err)
}

func testPayInvoice(client lnrpc.LightningClient, args []string) (string, error) {
	return TestCommand(client, payInvoiceCommand, payInvoice, "payinvoice", args)
}
