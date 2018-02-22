package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	addrToAmountJSON         = "{\"alpha\": 57}"
	addrToAmountMap          = map[string]int64{"alpha": 57}
	expectedSendManyResponse = "{\n    \"txid\": \"BitcoinTxid\"\n}\n"
)

func TestSendMany(t *testing.T) {
	TestCommandNoError(t, runSendMany,
		[]string{addrToAmountJSON},
		&lnrpc.SendManyRequest{AddrToAmount: addrToAmountMap},
		expectedSendManyResponse)
}

func TestSendMany_ConfTarget(t *testing.T) {
	TestCommandNoError(t, runSendMany,
		[]string{addrToAmountJSON, "--conf_target", "7"},
		&lnrpc.SendManyRequest{AddrToAmount: addrToAmountMap, TargetConf: 7},
		expectedSendManyResponse)
}

func TestSendMany_SatPerByte(t *testing.T) {
	TestCommandNoError(t, runSendMany,
		[]string{addrToAmountJSON, "--sat_per_byte", "22"},
		&lnrpc.SendManyRequest{AddrToAmount: addrToAmountMap, SatPerByte: 22},
		expectedSendManyResponse)
}

func TestSendMany_NoArgs(t *testing.T) {
	TestCommandTextInValidationError(t, runSendMany,
		[]string{},
		"unexpected end of JSON input")
}

func TestSendMany_BadJSON(t *testing.T) {
	TestCommandTextInValidationError(t, runSendMany,
		[]string{"BADJSON"},
		"invalid character")
}

func TestSendMany_BadConfTarget(t *testing.T) {
	TestCommandTextInResponse(t, runSendMany,
		[]string{"{}", "--conf_target", "BadConfigTarget"},
		"invalid syntax")
}

func TestSendMany_BadSatPerByte(t *testing.T) {
	TestCommandTextInResponse(t, runSendMany,
		[]string{"{}", "--sat_per_byte", "BadSatPerByte"},
		"invalid syntax")
}

func TestSendMany_MultipleFeeArgs(t *testing.T) {
	TestCommandValidationError(t, runSendMany,
		[]string{"{}", "--conf_target", "7", "--sat_per_byte", "22"},
		ErrMultipleFeeArgs)
}

func TestSendMany_SendFailure(t *testing.T) {
	TestCommandRPCError(t, runSendMany,
		[]string{"{}"},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runSendMany(client lnrpc.LightningClient, args []string) (string, error) {
	return RunCommand(
		client, sendManyCommand, sendMany, "sendmany", args)
}
