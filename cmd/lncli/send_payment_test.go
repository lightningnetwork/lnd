package main

import (
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

// Passing no arguments results in help/usage text.
func TestSendPayment_Usage(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testSendPayment(&client, []string{})
	require.NoError(t, err)
	require.True(t,
		strings.Contains(resp, "sendpayment - send a payment over lightning"))
}

// sendPayment can be used with a PayReq, just like payInvoice.
func TestSendPayment_PayReqFlag(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq
	testErrorlessSendPayment(t,
		[]string{"--pay_req", PayReq},
		&expectedSendRequest)
}

// An amount can be included with a PayReq.
func TestSendPayment_Amt(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.PaymentRequest = PayReq
	expectedSendRequest.Amt = 12000
	testErrorlessSendPayment(t,
		[]string{"--pay_req", PayReq, "--amt", "12000"},
		&expectedSendRequest)
}

// Errors on initiating a PayReq payment should be propagated up.
func TestSendPayment_SendPaymentError(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on initiating a PayReq payment are currently propagated up.
func TestSendPayment_SendPaymentEOF(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.EOF)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.EOF, err)
}

// Errors on sending a PayReq payment should be propagated up.
func TestSendPayment_StreamSendError(t *testing.T) {
	stream := NewSendPaymentStream(io.ErrClosedPipe, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on sending a PayReq payment are currently propagated up.
func TestSendPayment_StreamSendEOF(t *testing.T) {
	stream := NewSendPaymentStream(io.EOF, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.EOF, err)
}

// Errors on receiving confirmation of a PayReq payment should be propagated up.
func TestSendPayment_StreamRecvError(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.ErrClosedPipe)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// EOFs on receiving confirmation of a PayReq payment are currently propagated up.
func TestSendPayment_StreamRecvEOF(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.EOF)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.EOF, err)
}

// Dest, Amt, and PaymentHash can be specified as non-flag args.
func TestSendPayment_Dest_Amt_PaymentHash(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{
		Dest, PushAmount, PaymentHash})

	// Bug: No error should be returned here.
	// The payment_hash argument is not properly consumed,
	// causing final_cltv_delta to try to consume the payment_hash
	// as its own value, rather than consuming the next value.
	// TODO(merehap): Remove this line and uncomment the following lines.
	require.Error(t, err)
	//require.NoError(t, err)
	//require.Equal(t, SendPaymentResponse, resp)
	//require.Equal(t, expectedPaymentSendRequest(), stream.CapturedSendRequest)
}

// Dest, Amt, PaymentHash, and FinalCltvDelta can be specified as non-flag args.
func TestSendPayment_Dest_Amt_PaymentHash_FinalCltvDelta(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{
		Dest, PushAmount, PaymentHash, FinalCltvDelta})

	// TODO(merehap): See TestSendPayment_Dest_Amt_PaymentHash
	require.Error(t, err)
	//require.NoError(t, err)
	//require.Equal(t, SendPaymentResponse, resp)
	//require.Equal(t, expectedPaymentSendRequest(), stream.CapturedSendRequest)
}

// FinalCltvDelta can be specified as a stand-alone non-flag arg.
func TestSendPayment_FinalCltvDelta(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	expectedSendRequest.Amt = PushAmountInt
	expectedSendRequest.FinalCltvDelta = FinalCltvDeltaInt
	testErrorlessSendPayment(t,
		[]string{Dest, FinalCltvDelta, "--amt", PushAmount, "--payment_hash", PaymentHash},
		&expectedSendRequest)
}

// Dest can be specified as a flag.
func TestSendPayment_DestFlag(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	expectedSendRequest.Amt = PushAmountInt
	testErrorlessSendPayment(t,
		[]string{PushAmount, "--dest", Dest, "--payment_hash", PaymentHash},
		&expectedSendRequest)
}

// Amt can be specified as a flag.
func TestSendPayment_AmtFlag(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	expectedSendRequest.Amt = PushAmountInt
	testErrorlessSendPayment(t,
		[]string{Dest, "--amt", PushAmount, "--payment_hash", PaymentHash},
		&expectedSendRequest)
}

// PaymentHash can be specified as a flag.
func TestSendPayment_PaymentHashFlag(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	testErrorlessSendPayment(t,
		[]string{Dest, "--payment_hash", PaymentHash},
		&expectedSendRequest)
}

// FinalCltvDelta can be specified as a flag.
func TestSendPayment_FinalCltvDeltaFlag(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	expectedSendRequest.FinalCltvDelta = FinalCltvDeltaInt
	testErrorlessSendPayment(t,
		[]string{
			Dest,
			"--payment_hash", PaymentHash,
			"--final_cltv_delta", FinalCltvDelta},
		&expectedSendRequest)
}

// Passing all arguments as flags should be accepted.
func TestSendPayment_AllFlags(t *testing.T) {
	expectedSendRequest := expectedPaymentSendRequest()
	expectedSendRequest.Amt = PushAmountInt
	expectedSendRequest.FinalCltvDelta = FinalCltvDeltaInt
	testErrorlessSendPayment(t,
		[]string{
			"--dest", Dest,
			"--amt", PushAmount,
			"--payment_hash", PaymentHash,
			"--final_cltv_delta", FinalCltvDelta},
		&expectedSendRequest)
}

// Dest must be specified if PayReq isn't.
func TestSendPayment_NoDest(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{"--payment_hash", PaymentHash})

	require.Error(t, err)
	require.Equal(t, ErrMissingDestinationTxid, err)
}

// PaymentHash must be specified if PayReq isn't.
func TestSendPayment_NoPaymentHash(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{"--dest", Dest})

	require.Error(t, err)
	require.Equal(t, ErrMissingPaymentHash, err)
}

// Dest must be specified in a hexadecimal format.
func TestSendPayment_NonHexDest(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{"ABC"})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "encoding/hex: odd length hex string"),
		"Wrong error returned: "+err.Error())
}

// Dest must be the correct length.
func TestSendPayment_BadHexLengthDest(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{"ABCD"})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "dest node pubkey must be exactly 33 bytes, is instead:"),
		"Wrong error returned: "+err.Error())
}

// Amts must be numbers.
func TestSendPayment_BadAmt(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{
		Dest,
		"BadAmount",
		"--payment_hash", PaymentHash})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "unable to decode payment amount"),
		"Wrong error returned.")
}

// Amts must be numbers.
func TestSendPayment_BadAmtFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testSendPayment(&client, []string{
		Dest,
		"--amt", "BadAmount",
		"--payment_hash", PaymentHash})

	require.NoError(t, err)
	require.True(t,
		strings.Contains(resp, "sendpayment - send a payment over lightning"),
		"Wrong error returned.")
}

// PaymentHashes must be hexadecimal.
func TestSendPayment_NonHexPaymentHash(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{Dest, PushAmount, "ABC"})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "encoding/hex: odd length hex string"),
		"Wrong error returned."+err.Error())
}

// PaymentHashes must have the correct length.
func TestSendPayment_BadHexLengthPaymentHash(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{Dest, PushAmount, "ABCD"})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "payment hash must be exactly 32 bytes, is instead"),
		"Wrong error returned: "+err.Error())
}

// PaymentHashes must be hexadecimal.
func TestSendPayment_NonHexPaymentHashFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{Dest, "--payment_hash", "ABC"})

	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "encoding/hex: odd length hex string"),
		"Wrong error returned."+err.Error())
}

// FinalCltvDeltas must be integers.
func TestSendPaymentBadFinalCltvDelta(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendPayment(&client, []string{
		"--dest", Dest,
		"--amt", PushAmount,
		"--payment_hash", PaymentHash,
		"BadFinalCltvDelta"})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "invalid syntax"),
		"Wrong error returned."+err.Error())
}

// FinalCltvDeltas must be integers.
func TestSendPaymentBadFinalCltvDeltaFlag(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	resp, err := testSendPayment(&client, []string{
		Dest,
		"--payment_hash", PaymentHash,
		"--final_cltv_delta", "BadFinalCltvDelta"})
	require.NoError(t, err)
	require.True(t,
		strings.Contains(resp, "sendpayment - send a payment over lightning"),
		"Wrong error returned."+resp)
}

// Payments can be test-sent with no PaymentHash.
func TestSendPayment_DebugSend(t *testing.T) {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.Dest = DestBytes
	expectedSendRequest.Amt = PushAmountInt
	testErrorlessSendPayment(t,
		[]string{Dest, PushAmount, "--debug_send"},
		&expectedSendRequest)
}

// PaymentHash is pointless for debug sends.
func TestSendPayment_DebugSendWithPaymentHash(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{
		Dest, PushAmount, "--payment_hash", PaymentHash, "--debug_send"})

	require.Error(t, err)
	require.Equal(t, "do not provide a payment hash with debug send", err.Error())
}

// Specifying additional arguments is pointless for debug sends.
func TestSendPayment_DebugSendWithArgs(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := testSendPayment(&client, []string{
		Dest, PushAmount, PaymentHash, "--debug_send"})

	require.Error(t, err)
	require.Equal(t, "do not provide a payment hash with debug send", err.Error())
}

func testSendPayment(client lnrpc.LightningClient, args []string) (string, error) {
	return TestCommand(client, sendPaymentCommand, sendPayment, "sendpayment", args)
}

// Test that cases that aren't expected to error return the correct output.
func testErrorlessSendPayment(
	t *testing.T, args []string, expectedSendRequest *lnrpc.SendRequest) {

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := testSendPayment(&client, args)

	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
	require.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
}

func expectedPaymentSendRequest() lnrpc.SendRequest {
	expectedSendRequest := lnrpc.SendRequest{}
	expectedSendRequest.Dest = DestBytes
	expectedSendRequest.PaymentHash = PaymentHashBytes
	return expectedSendRequest
}
