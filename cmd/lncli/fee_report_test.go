package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedFeeReportResponse = "{\n    \"channel_fees\": [\n        {\n            " +
		"\"channel_point\": \"ChanPoint\",\n            " +
		"\"base_fee_msat\": \"789\",\n            " +
		"\"fee_per_mil\": \"456\",\n            " +
		"\"fee_rate\": 123\n        }\n    ]\n}\n"
)

func TestFeeReport(t *testing.T) {
	TestCommandNoError(t, runFeeReport,
		[]string{},
		&lnrpc.FeeReportRequest{},
		expectedFeeReportResponse)
}

func TestFeeReport_RPCError(t *testing.T) {
	TestCommandRPCError(t, runFeeReport,
		[]string{},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runFeeReport(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, feeReportCommand, feeReport, "feereport", args)
}
