package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var expectedResponse = "{\n    \"txid\": \"BitcoinTxid\"\n}\n"

// Check that the basic arguments are passed through and that
// a txid should be printed to the screen.
func TestSendCoins(t *testing.T) {
	TestCommandNoError(t, runSendCoins,
		[]string{BitcoinAddress, PushAmount},
		&lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0},
		expectedResponse)
}

// Address can be specified through a flag.
func TestSendCoins_AddrFlag(t *testing.T) {
	TestCommandNoError(t, runSendCoins,
		[]string{"--addr", BitcoinAddress, PushAmount},
		&lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0},
		expectedResponse)
}

// Amt can be specified through a flag.
func TestSendCoins_AmtFlag(t *testing.T) {
	TestCommandNoError(t, runSendCoins,
		[]string{"--amt", PushAmount, BitcoinAddress},
		&lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 0},
		expectedResponse)
}

// ConfTarget should be passed through to the LightningClient.
func TestSendCoins_ConfTarget(t *testing.T) {
	TestCommandNoError(t, runSendCoins,
		[]string{"--conf_target", "7", BitcoinAddress, PushAmount},
		&lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 7, 0},
		expectedResponse)
}

// SatPerByte should be passed through to the LightningClient.
func TestSendCoins_SatPerByte(t *testing.T) {
	TestCommandNoError(t, runSendCoins,
		[]string{"--sat_per_byte", "22", BitcoinAddress, PushAmount},
		&lnrpc.SendCoinsRequest{BitcoinAddress, PushAmountInt, 0, 22},
		expectedResponse)
}

// Usage should be provided if no arguments are specified.
func TestSendCoins_Usage(t *testing.T) {
	TestCommandTextInResponse(t, runSendCoins,
		[]string{},
		"sendcoins - send bitcoin on-chain to an address")
}

// Address is required.
func TestSendCoins_NoAddr(t *testing.T) {
	TestCommandValidationError(t, runSendCoins,
		[]string{"--amt", PushAmount},
		ErrMissingAddress)
}

// Amt is required.
func TestSendCoins_NoAmt(t *testing.T) {
	TestCommandValidationError(t, runSendCoins,
		[]string{BitcoinAddress},
		ErrMissingAmount)
}

// Amt must be an integer.
func TestSendCoins_BadAmtArg(t *testing.T) {
	TestCommandTextInValidationError(t, runSendCoins,
		[]string{BitcoinAddress, "BadAmt"},
		"unable to decode amount:")
}

// Amt must be an integer.
// This should generate an error, but instead prints the usage text.
func TestSendCoins_BadAmtFlag(t *testing.T) {
	TestCommandTextInResponse(t, runSendCoins,
		[]string{"--amt", "BadAmt", BitcoinAddress},
		"for flag -amt: strconv.ParseInt: parsing")
}

// Fees must only be specified in one of two conflicting ways.
func TestSendCoins_BothFeeOptions(t *testing.T) {
	TestCommandValidationError(t, runSendCoins,
		[]string{"--conf_target", "7", "--sat_per_byte", "22"},
		ErrMultipleFeeArgs)
}

// ConfTarget must be an integer.
func TestSendCoins_BadConfTarget(t *testing.T) {
	TestCommandTextInResponse(t, runSendCoins,
		[]string{"--conf_target", "BadConfTarget", BitcoinAddress, PushAmount},
		"for flag -conf_target: strconv.ParseInt")
}

// SatPerByte must be an integer.
func TestSendCoins_BadSatPerByte(t *testing.T) {
	TestCommandTextInResponse(t, runSendCoins,
		[]string{"--sat_per_byte", "BadSatPerByte", BitcoinAddress, PushAmount},
		"for flag -sat_per_byte: strconv.ParseInt")
}

// RPC failure is propagated up.
func TestSendCoins_SendFailure(t *testing.T) {
	TestCommandRPCError(t, runSendCoins,
		[]string{BitcoinAddress, PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runSendCoins(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, sendCoinsCommand, sendCoins, "sendcoins", args)
}
