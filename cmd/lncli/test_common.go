package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/urfave/cli"
)

var (
	CommandTimeout = 50 * time.Millisecond
	ErrTimeout     = fmt.Errorf("Timed out waiting for the command to complete.")

	PubKey                 = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"
	GoodAddress            = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org:1234"
	GoodAddressWithoutPort = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org"
	BadAddress             = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"

	PeerIdInt      int32 = 321
	PeerId               = "321"
	LocalAmountInt int64 = 10000
	LocalAmount          = "10000"
	PushAmountInt  int64 = 5000
	PushAmount           = "5000"

	FundingTxidString        = "1234567890000000000000000000000000000000000000000000000000000000"
	OutputIndex              = "555"
	OutputIndexInt    uint32 = 555
	TimeLimit                = "42"
)

type StringWriter struct {
	outputs []string
}

func (w *StringWriter) Write(p []byte) (n int, err error) {
	w.outputs = append(w.outputs, string(p))
	return len(p), nil
}

func (w *StringWriter) Join() string {
	return strings.Join(w.outputs, "\n")
}

// Calls the specified command with the specified LightningClient and args.
// Replaces stdout as the writer so that the output can be unit tested (without IO).
// Applies a timeout to the command call to prevent infinite looping
// since unit tests should terminate, even if non-termination would indicate a bug.
func TestCommandWithTimeout(
	client lnrpc.LightningClient,
	command cli.Command,
	commandAction func(*cli.Context, lnrpc.LightningClient, io.Writer) error,
	commandName string,
	args []string) (string, error) {

	app := cli.NewApp()
	writer := StringWriter{}
	// Redirect the command output from stdout to a writer we can test.
	app.Writer = &writer

	// The actual command causes real network events and
	// prints to the console. For testing purposes we need to override
	// this functionality to stub out the network events and write to
	// a Writer that we can validate.
	var err error
	command.Action = func(context *cli.Context) {
		err = commandAction(context, client, &writer)
	}

	app.Commands = []cli.Command{command}
	args = append([]string{"lncli", commandName}, args...)
	// A go channel is needed to tell when the command has ended.
	// Commands that use TestCommandWithTimeout are using it because
	// they can contain infinitely loops, so they need to be run on a
	// separate thread with a timeout.
	channel := make(chan string, 1)
	go func() {
		app.Run(args)
		channel <- "goroutine terminated"
	}()

	select {
	case <-channel:
		// closeChannel completed within the timeout
		if err != nil {
			return "", err
		}

		return writer.Join(), nil
	case <-time.After(CommandTimeout):
		// The command was blocking (probably) indefinitely,
		// which it is currently intended only if no EOF nor error occurred.
		return "", ErrTimeout
	}
}
