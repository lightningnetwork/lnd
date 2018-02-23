package main

import (
	"io"
	"testing"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func TestStopDaemon(t *testing.T) {
	TestCommandNoError(t, runStopDaemon,
		[]string{},
		&lnrpc.StopRequest{},
		"")
}

func TestStopDaemon_RPCError(t *testing.T) {
	TestCommandRPCError(t, runStopDaemon,
		[]string{PushAmount},
		io.ErrClosedPipe,
		io.ErrClosedPipe)
}

func runStopDaemon(
	client lnrpc.LightningClient, args []string) (string, error) {

	return RunCommand(
		client, stopCommand, stopDaemon, "stop", args)
}
