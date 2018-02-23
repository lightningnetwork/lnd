package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

var (
	expectedDebugLevelResponse = "{\n    \"sub_systems\": \"\"\n}\n"
)

func TestDebugLevel(t *testing.T) {
	TestCommandNoError(t, runDebugLevel,
		[]string{},
		&lnrpc.DebugLevelRequest{Show: false, LevelSpec: ""},
		expectedDebugLevelResponse)
}

func TestDebugLevel_AllFlags(t *testing.T) {
	TestCommandNoError(t, runDebugLevel,
		[]string{"--show", "--level", "HIGH"},
		&lnrpc.DebugLevelRequest{Show: true, LevelSpec: "HIGH"},
		expectedDebugLevelResponse)
}

func TestDebugLevel_RPCError(t *testing.T) {
	TestCommandRPCError(t, runDebugLevel,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runDebugLevel(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, debugLevelCommand, debugLevel, "debuglevel", args)
}
