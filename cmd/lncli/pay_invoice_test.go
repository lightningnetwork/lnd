package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/assert"
	"golang.org/x/net/context"
)

var (
	PayInvoiceResponse = "{\n\t\"payment_error\": \"ERROR\"," +
		"\n\t\"payment_preimage\": \"0c22384e\"," +
		"\n\t\"payment_route\": {" +
		"\n\t\t\"total_time_lock\": 2," +
		"\n\t\t\"total_fees\": 5," +
		"\n\t\t\"total_amt\": 8," +
		"\n\t\t\"hops\": [\n\t\t\t" +
		"{\n\t\t\t\t\"chan_capacity\": 1," +
		"\n\t\t\t\t\"amt_to_forward\": 2," +
		"\n\t\t\t\t\"fee\": 3," +
		"\n\t\t\t\t\"expiry\": 4\n\t\t\t},\n\t\t\t" +
		"{\n\t\t\t\t\"chan_id\": 5," +
		"\n\t\t\t\t\"chan_capacity\": 6," +
		"\n\t\t\t\t\"amt_to_forward\": 7," +
		"\n\t\t\t\t\"fee\": 8," +
		"\n\t\t\t\t\"expiry\": 9\n\t\t\t}\n\t\t]\n\t}\n}\n"

	PayReq = "MyPayReq"
)

// Verify that that PayReq is passed to Stream.SendMsg() and that the correct
// PayInvoiceResponse is returned.
func TestPayInvoice(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq

	stream := NewPayInvoiceStream(nil, nil)
	client := newPayInvoiceLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{PayReq})

	assert.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
	assert.NoError(t, err)
	assert.Equal(t, PayInvoiceResponse, resp)
}

// PayReq can be passed as a flag.
func TestPayInvoice_PayReqFlag(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq

	stream := NewPayInvoiceStream(nil, nil)
	client := newPayInvoiceLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{"--pay_req", PayReq})

	assert.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
	assert.NoError(t, err)
	assert.Equal(t, PayInvoiceResponse, resp)
}

// A PayReq can include an amount.
func TestPayInvoice_Amt(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq
	expectedSendRequest.Amt = 12000

	stream := NewPayInvoiceStream(nil, nil)
	client := newPayInvoiceLightningClient(&stream)
	resp, err := testPayInvoice(&client, []string{"--amt", "12000", PayReq})

	assert.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
	assert.NoError(t, err)
	assert.Equal(t, PayInvoiceResponse, resp)
}

// PayReq is required.
func TestPayInvoice_NoPayReq(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testPayInvoice(&client, []string{})
	assert.Equal(t, ErrMissingPayReq, err)
}

// Errors on initiating a payment should be propagated up.
func TestPayInvoice_SendPaymentError(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on initiating a payment are currently propagated up.
func TestPayInvoice_SendPaymentEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.EOF, err)
}

// Errors on sending a payment should be propagated up.
func TestPayInvoice_StreamSendError(t *testing.T) {
	stream := NewPayInvoiceStream(io.ErrClosedPipe, nil)
	client := newPayInvoiceLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on sending a payment are currently propagated up.
func TestPayInvoice_StreamSendEOF(t *testing.T) {
	stream := NewPayInvoiceStream(io.EOF, nil)
	client := newPayInvoiceLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.EOF, err)
}

// Errors on receiving confirmation of a payment should be propagated up.
func TestPayInvoice_StreamRecvError(t *testing.T) {
	stream := NewPayInvoiceStream(nil, io.ErrClosedPipe)
	client := newPayInvoiceLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on receiving confirmation of a payment are currently propagated up.
func TestPayInvoice_StreamRecvEOF(t *testing.T) {
	stream := NewPayInvoiceStream(nil, io.EOF)
	client := newPayInvoiceLightningClient(&stream)
	_, err := testPayInvoice(&client, []string{PayReq})
	assert.Equal(t, io.EOF, err)
}

func testPayInvoice(client lnrpc.LightningClient, args []string) (string, error) {
	return TestCommand(client, payInvoiceCommand, payInvoice, "payinvoice", args)
}

func newPayInvoiceLightningClient(stream *payInvoiceStream) lnrpctesting.StubLightningClient {
	client := lnrpctesting.NewStubLightningClient()
	clientStream := lnrpctesting.StubClientStream{stream}
	client.SendPaymentClient = lnrpctesting.StubSendPaymentClient{&clientStream}
	return client
}

// A Stream that allows setting errors on send and recv,
// or returning a specified SendResponse
// while capturing the SendRequest that is received so it can be verified in tests.
type payInvoiceStream struct {
	SendError           error
	RecvError           error
	SendResponse        *lnrpc.SendResponse
	CapturedSendRequest lnrpc.SendRequest
}

func NewPayInvoiceStream(sendError error, recvError error) payInvoiceStream {
	hop1 := lnrpc.Hop{0, 1, 2, 3, 4}
	hop2 := lnrpc.Hop{5, 6, 7, 8, 9}
	hops := []*lnrpc.Hop{&hop1, &hop2}
	route := lnrpc.Route{2, 5, 8, hops}
	sendResponse := lnrpc.SendResponse{"ERROR", []byte{12, 34, 56, 78}, &route}
	return payInvoiceStream{sendError, recvError, &sendResponse, lnrpc.SendRequest{}}
}

func (s *payInvoiceStream) Context() context.Context {
	return new(lnrpctesting.StubContext)
}

func (s *payInvoiceStream) SendMsg(m interface{}) error {
	result, ok := m.(*lnrpc.SendRequest)
	if ok {
		s.CapturedSendRequest = *result
	}

	return s.SendError
}

func (s *payInvoiceStream) RecvMsg(m interface{}) error {
	result, ok := m.(*lnrpc.SendResponse)

	if ok {
		result.PaymentError = s.SendResponse.PaymentError
		result.PaymentPreimage = s.SendResponse.PaymentPreimage
		result.PaymentRoute = s.SendResponse.PaymentRoute
	}

	return s.RecvError
}
