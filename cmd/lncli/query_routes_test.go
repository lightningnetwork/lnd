package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedQueryRoutesResponse = "{\n    \"routes\": [\n        {\n            " +
		"\"total_time_lock\": 123,\n            " +
		"\"total_fees\": \"456\",\n            " +
		"\"total_amt\": \"789\",\n            " +
		"\"hops\": [\n            ]\n        }\n    ]\n}\n"
)

func TestQueryRoutes(t *testing.T) {
	TestCommandNoError(t, runQueryRoutes,
		[]string{PubKey, PushAmount},
		expectedQueryRoutesRequest(),
		expectedQueryRoutesResponse)
}

func TestQueryRoutes_DestFlag(t *testing.T) {
	TestCommandNoError(t, runQueryRoutes,
		[]string{"--dest", PubKey, PushAmount},
		expectedQueryRoutesRequest(),
		expectedQueryRoutesResponse)
}

func TestQueryRoutes_AmtFlag(t *testing.T) {
	TestCommandNoError(t, runQueryRoutes,
		[]string{PubKey, "--amt", PushAmount},
		expectedQueryRoutesRequest(),
		expectedQueryRoutesResponse)
}

func TestQueryRoutes_NoDest(t *testing.T) {
	TestCommandValidationError(t, runQueryRoutes,
		[]string{"--amt", PushAmount},
		ErrMissingDest)
}

func TestQueryRoutes_NoAmt(t *testing.T) {
	TestCommandValidationError(t, runQueryRoutes,
		[]string{"--dest", PubKey},
		ErrMissingAmount)
}

func TestQueryRoutes_RPCError(t *testing.T) {
	TestCommandRPCError(t, runQueryRoutes,
		[]string{PubKey, PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func TestQueryRoutes_BadAmt(t *testing.T) {
	TestCommandTextInValidationError(t, runQueryRoutes,
		[]string{PubKey, "BadPushAmount"},
		"unable to decode amt argument:")
}

func TestQueryRoutes_BadAmtFlag(t *testing.T) {
	TestCommandTextInResponse(t, runQueryRoutes,
		[]string{PubKey, "--amt", "BadPushAmount"},
		"queryroutes - Query a route to a destination.")
}

func runQueryRoutes(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, queryRoutesCommand, queryRoutes, "queryroutes", args)
}

func expectedQueryRoutesRequest() *lnrpc.QueryRoutesRequest {
	return &lnrpc.QueryRoutesRequest{
		PubKey:    "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4",
		Amt:       5000,
		NumRoutes: 10}
}
