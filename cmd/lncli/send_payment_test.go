package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/stretchr/testify/require"
)

// TestSendPayment_Usage verifies that passing no arguments results
// in help/usage text.
func TestSendPayment_Usage(t *testing.T) {
	TestCommandTextInResponse(t, runSendPayment,
		[]string{},
		"sendpayment - Send a payment over lightning")
}

// TestSendPayment_PayReqFlag verifies that sendPayment can be used
// with a PayReq, just like payInvoice.
func TestSendPayment_PayReqFlag(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{"--pay_req", PayReq},
		&lnrpc.SendRequest{PaymentRequest: PayReq})
}

// TestSendPayment_Amt verifies that an amount can be included with a PayReq.
func TestSendPayment_Amt(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{"--pay_req", PayReq, "--amt", "12000"},
		&lnrpc.SendRequest{PaymentRequest: PayReq, Amt: 12000})
}

// TestSendPayment_SendPaymentError verifies that errors on initiating
// a PayReq payment should be propagated up.
func TestSendPayment_SendPaymentError(t *testing.T) {
	TestCommandRPCError(t, runSendPayment,
		[]string{"--pay_req", PayReq},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

// TestSendPayment_StreamSendError verifies that errors on sending a PayReq
// payment should be propagated up.
func TestSendPayment_StreamSendError(t *testing.T) {
	stream := NewSendPaymentStream(io.ErrClosedPipe, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// TestSendPayment_StreamRecvError verifies that errors on receiving
// confirmation of a PayReq payment should be propagated up.
func TestSendPayment_StreamRecvError(t *testing.T) {
	stream := NewSendPaymentStream(nil, io.ErrClosedPipe)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{"--pay_req", PayReq})
	require.Equal(t, io.ErrClosedPipe, err)
}

// TestSendPayment_Dest_Amt_PaymentHash verifies that
// Dest, Amt, and PaymentHash can be specified as non-flag args.
func TestSendPayment_Dest_Amt_PaymentHash(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{
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

// TestSendPayment_Dest_Amt_PaymentHash_FinalCltvDelta verifies that
// Dest, Amt, PaymentHash, and FinalCltvDelta can be specified as non-flag args.
func TestSendPayment_Dest_Amt_PaymentHash_FinalCltvDelta(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{
		Dest, PushAmount, PaymentHash, FinalCltvDelta})

	// TODO(merehap): See TestSendPayment_Dest_Amt_PaymentHash
	require.Error(t, err)
	//require.NoError(t, err)
	//require.Equal(t, SendPaymentResponse, resp)
	//require.Equal(t, expectedPaymentSendRequest(), stream.CapturedSendRequest)
}

// TestSendPayment_FinalCltvDelta verifies that FinalCltvDelta can be specified
// as a stand-alone non-flag arg.
func TestSendPayment_FinalCltvDelta(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			Dest,
			FinalCltvDelta,
			"--amt", PushAmount,
			"--payment_hash", PaymentHash},
		&lnrpc.SendRequest{
			Dest:           DestBytes,
			PaymentHash:    PaymentHashBytes,
			Amt:            PushAmountInt,
			FinalCltvDelta: FinalCltvDeltaInt})
}

// TestSendPayment_DestFlag verifies that Dest can be specified as a flag.
func TestSendPayment_DestFlag(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			PushAmount,
			"--dest", Dest,
			"--payment_hash", PaymentHash},
		&lnrpc.SendRequest{
			Dest:        DestBytes,
			PaymentHash: PaymentHashBytes,
			Amt:         PushAmountInt})
}

// TestSendPayment_AmtFlag verifies that Amt can be specified as a flag.
func TestSendPayment_AmtFlag(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			Dest,
			"--amt", PushAmount,
			"--payment_hash", PaymentHash},
		&lnrpc.SendRequest{
			Dest:        DestBytes,
			PaymentHash: PaymentHashBytes,
			Amt:         PushAmountInt})
}

// TestSendPayment_PaymentHashFlag verifies that PaymentHash can be
// specified as a flag.
func TestSendPayment_PaymentHashFlag(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			Dest,
			"--payment_hash", PaymentHash},
		&lnrpc.SendRequest{
			Dest:        DestBytes,
			PaymentHash: PaymentHashBytes})
}

// TestSendPayment_FinalCltvDeltaFlag verifies that FinalCltvDelta can be
// specified as a flag.
func TestSendPayment_FinalCltvDeltaFlag(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			Dest,
			"--payment_hash", PaymentHash,
			"--final_cltv_delta", FinalCltvDelta},
		&lnrpc.SendRequest{
			Dest:           DestBytes,
			PaymentHash:    PaymentHashBytes,
			FinalCltvDelta: FinalCltvDeltaInt})
}

// TestSendPayment_AllFlags verifies that passing all arguments as flags
// should be accepted.
func TestSendPayment_AllFlags(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{
			"--dest", Dest,
			"--amt", PushAmount,
			"--payment_hash", PaymentHash,
			"--final_cltv_delta", FinalCltvDelta},
		&lnrpc.SendRequest{
			Dest:           DestBytes,
			PaymentHash:    PaymentHashBytes,
			Amt:            PushAmountInt,
			FinalCltvDelta: FinalCltvDeltaInt})
}

// TestSendPayment_NoDest verifies that Dest must be specified if PayReq isn't.
func TestSendPayment_NoDest(t *testing.T) {
	TestCommandValidationError(t, runSendPayment,
		[]string{"--payment_hash", PaymentHash},
		ErrMissingDestinationTxid)
}

// TestSendPayment_NoPaymentHash verifies that PaymentHash must be specified
// if PayReq isn't.
func TestSendPayment_NoPaymentHash(t *testing.T) {
	TestCommandValidationError(t, runSendPayment,
		[]string{"--dest", Dest},
		ErrMissingPaymentHash)
}

// TestSendPayment_NonHexDest verifiest that Dest must be specified
// in a hexadecimal format.
func TestSendPayment_NonHexDest(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{"ABC"},
		"encoding/hex: odd length hex string")
}

// TestSendPayment_BadHexLengthDest verifies that Dest must be the
// correct length.
func TestSendPayment_BadHexLengthDest(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{"ABCD"},
		"dest node pubkey must be exactly 33 bytes, is instead:")
}

// TestSendPayment_BadAmt verifies that Amts must be numbers.
func TestSendPayment_BadAmt(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{Dest, "BadAmount", "--payment_hash", PaymentHash},
		"unable to decode payment amount")
}

// TestSendPayment_BadAmtFlag verifies that Amts must be numbers.
func TestSendPayment_BadAmtFlag(t *testing.T) {
	TestCommandTextInResponse(t, runSendPayment,
		[]string{Dest, "--amt", "BadAmount", "--payment_hash", PaymentHash},
		"Incorrect Usage: invalid value")
}

// TestSendPayment_NonHexPaymentHash verifies that PaymentHashes
// must be hexadecimal.
func TestSendPayment_NonHexPaymentHash(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{Dest, PushAmount, "ABC"},
		"encoding/hex: odd length hex string")
}

// TestSendPayment_BadHexLengthPaymentHash verifies that PaymentHashes
// must have the correct length.
func TestSendPayment_BadHexLengthPaymentHash(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{Dest, PushAmount, "ABCD"},
		"payment hash must be exactly 32 bytes, is instead")
}

// TestSendPayment_NonHexPaymentHashFlag verifies that PaymentHashes
// must be hexadecimal.
func TestSendPayment_NonHexPaymentHashFlag(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{Dest, "--payment_hash", "ABC"},
		"encoding/hex: odd length hex string")
}

// TestSendPaymentBadFinalCltvDelta verifies that FinalCltvDeltas
// must be integers.
func TestSendPaymentBadFinalCltvDelta(t *testing.T) {
	TestCommandTextInValidationError(t, runSendPayment,
		[]string{
			"--dest", Dest,
			"--amt", PushAmount,
			"--payment_hash", PaymentHash,
			"BadFinalCltvDelta"},
		"invalid syntax")
}

// TestSendPaymentBadFinalCltvDeltaFlag verifies that FinalCltvDeltas
// must be integers.
func TestSendPaymentBadFinalCltvDeltaFlag(t *testing.T) {
	TestCommandTextInResponse(t, runSendPayment,
		[]string{
			Dest,
			"--payment_hash", PaymentHash,
			"--final_cltv_delta", "BadFinalCltvDelta"},
		"Incorrect Usage: invalid value")
}

// TestSendPayment_DebugSend verifies that payments can be test-sent
// with no PaymentHash.
func TestSendPayment_DebugSend(t *testing.T) {
	testErrorlessSendPayment(t,
		[]string{Dest, PushAmount, "--debug_send"},
		&lnrpc.SendRequest{Dest: DestBytes, Amt: PushAmountInt})
}

// TestSendPayment_DebugSendWithPaymentHash verifies that PaymentHash is
// pointless for debug sends.
func TestSendPayment_DebugSendWithPaymentHash(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{
		Dest, PushAmount, "--payment_hash", PaymentHash, "--debug_send"})

	require.Error(t, err)
	require.Equal(t, ErrUnnecessaryArgumentForDebugSend, err)
}

// TestSendPayment_DebugSendWithArgs verifies that specifying
// additional arguments is pointless for debug sends.
func TestSendPayment_DebugSendWithArgs(t *testing.T) {
	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	_, err := runSendPayment(&client, []string{
		Dest, PushAmount, PaymentHash, "--debug_send"})

	require.Error(t, err)
	require.Equal(t, ErrUnnecessaryArgumentForDebugSend, err)
}

func runSendPayment(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(client, sendPaymentCommand, sendPayment, "sendpayment", args)
}

// Test that the payment stream receives the correct input and returns the correct output.
// TestCommandNoError can't be used for this purpose since it checks the response
// of the RPC call, not the stream call.
func testErrorlessSendPayment(
	t *testing.T, args []string, expectedSendRequest *lnrpc.SendRequest) {

	stream := NewSendPaymentStream(nil, nil)
	client := NewSendPaymentLightningClient(&stream)
	resp, err := runSendPayment(&client, args)

	require.NoError(t, err)
	require.Equal(t, SendPaymentResponse, resp)
	require.Equal(t, expectedSendRequest, stream.CapturedSendRequest)
}
