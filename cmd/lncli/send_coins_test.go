package main

import (
	"io"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
)

// Check that the basic arguments are passed through and that
// a txid should be printed to the screen.
func TestSendCoins(t *testing.T) {
	testErrorlessSendCoins(t,
		[]string{BitcoinAddress, PushAmount},
		lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0})
}

// Address can be specified through a flag.
func TestSendCoins_AddrFlag(t *testing.T) {
	testErrorlessSendCoins(t,
		[]string{"--addr", BitcoinAddress, PushAmount},
		lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0})
}

// Amt can be specified through a flag.
func TestSendCoins_AmtFlag(t *testing.T) {
	testErrorlessSendCoins(t,
		[]string{"--amt", PushAmount, BitcoinAddress},
		lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0})
}

// ConfTarget should be passed through to the LightningClient.
func TestSendCoins_ConfTarget(t *testing.T) {
	testErrorlessSendCoins(t,
		[]string{"--conf_target", "7", BitcoinAddress, PushAmount},
		lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 7, 0})
}

// SatPerByte should be passed through to the LightningClient.
func TestSendCoins_SatPerByte(t *testing.T) {
	testErrorlessSendCoins(t,
		[]string{"--sat_per_byte", "22", BitcoinAddress, PushAmount},
		lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 22})
}

// Usage should be provided if no arguments are specified.
func TestSendCoins_Usage(t *testing.T) {
	testTextContainedInSendCoinsResponse(t,
		[]string{},
		"sendcoins - send bitcoin on-chain to an address")
}

// Address is required.
func TestSendCoins_NoAddr(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendCoins(&client, []string{"--amt", PushAmount})
	require.Equal(t, ErrMissingAddress, err)
}

// Amt is required.
func TestSendCoins_NoAmt(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendCoins(&client, []string{BitcoinAddress})
	require.Equal(t, ErrMissingAmount, err)
}

// Amt must be an integer.
func TestSendCoins_BadAmtArg(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendCoins(&client,
		[]string{BitcoinAddress, "BadAmt"})
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), "unable to decode amount:"))
}

// Amt must be an integer.
func TestSendCoins_BadAmtFlag(t *testing.T) {
	testTextContainedInSendCoinsResponse(t,
		[]string{"--amt", "BadAmt", BitcoinAddress},
		"for flag -amt: strconv.ParseInt: parsing")
}

// Fees must only be specified in one of two conflicting ways.
func TestSendCoins_BothFeeOptions(t *testing.T) {
	client := lnrpctesting.NewStubLightningClient()
	_, err := testSendCoins(&client,
		[]string{"--conf_target", "7", "--sat_per_byte", "22"})
	require.Equal(t, ErrMultipleFeeArgs, err)
}

// ConfTarget must be an integer.
func TestSendCoins_BadConfTarget(t *testing.T) {
	testTextContainedInSendCoinsResponse(t,
		[]string{"--conf_target", "BadConfTarget", BitcoinAddress, PushAmount},
		"for flag -conf_target: strconv.ParseInt")
}

// SatPerByte must be an integer.
func TestSendCoins_BadSatPerByte(t *testing.T) {
	testTextContainedInSendCoinsResponse(t,
		[]string{"--sat_per_byte", "BadSatPerByte", BitcoinAddress, PushAmount},
		"for flag -sat_per_byte: strconv.ParseInt")
}

// RPC failure is propagated up.
func TestSendCoins_SendFailure(t *testing.T) {
	client := lnrpctesting.NewFailingStubLightningClient(io.ErrClosedPipe)
	_, err := testSendCoins(&client,
		[]string{BitcoinAddress, PushAmount})
	require.Equal(t, io.ErrClosedPipe, err)
}

func testSendCoins(
	client lnrpc.LightningClient, args []string) (string, error) {

	return TestCommand(
		client, sendCoinsCommand, sendCoins, "sendcoins", args)
}

// Verify that the expected output and SendCoinsRequest are generated
// for the specified command line args.
func testErrorlessSendCoins(
	t *testing.T,
	args []string,
	expectedRequest lnrpc.SendCoinsRequest) {

	client := lnrpctesting.NewStubLightningClient()
	resp, err := testSendCoins(&client, args)
	require.NoError(t, err)
	require.Equal(t, "{\n    \"txid\": \"BitcoinTxid\"\n}\n", resp)

	require.Equal(t, &expectedRequest, client.CapturedSendCoinsRequest)
}

// Verify that the specified text is present when the specified
// command line args are passed.
func testTextContainedInSendCoinsResponse(
	t *testing.T, args []string, expectedText string) {

	client := lnrpctesting.NewStubLightningClient()
	resp, err := testSendCoins(&client, args)
	require.NoError(t, err)
	require.True(t, strings.Contains(resp, expectedText))
}
