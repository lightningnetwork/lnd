package main

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/testing"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
	"golang.org/x/net/context"
)

var (
	commandTimeout = 50 * time.Millisecond
	ErrTimeout     = fmt.Errorf("Timed out waiting for the command to complete.")

	PubKey                 = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"
	GoodAddress            = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org:1234"
	GoodAddressWithoutPort = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4@bitcoin.org"
	BadAddress             = "02c39955c1579afe4824dc0ef4493fdf7f3760b158cf6d367d8570b9f19683afb4"
	Host                   = "bitcoin.org"
	HostWithPort           = "bitcoin.org:1234"

	PeerIDInt      int32 = 321
	PeerID               = "321"
	LocalAmountInt int64 = 10000
	LocalAmount          = "10000"
	PushAmountInt  int64 = 5000
	PushAmount           = "5000"

	FundingTxidString        = "1234567890000000000000000000000000000000000000000000000000000000"
	OutputIndex              = "555"
	OutputIndexInt    uint32 = 555
	TimeLimit                = "42"

	PayReq           = "MyPayReq"
	Dest             = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef01"
	DestBytes        = []byte{0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1}
	PaymentHash      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	PaymentHashBytes = []byte{0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x1, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef}

	FinalCltvDeltaInt int32 = 21
	FinalCltvDelta          = "21"

	BitcoinAddress = "BitcoinAddress"

	SendPaymentResponse = "{\n\t\"payment_error\": \"ERROR\"," +
		"\n\t\"payment_preimage\": \"0c22384e\"," +
		"\n\t\"payment_route\": {" +
		"\n\t\t\"total_time_lock\": 2," +
		"\n\t\t\"total_fees\": 5," +
		"\n\t\t\"total_amt\": 8," +
		"\n\t\t\"hops\": [\n\t\t\t" +
		"{\n\t\t\t\t\"chan_capacity\": 1," +
		"\n\t\t\t\t\"amt_to_forward\": 2," +
		"\n\t\t\t\t\"fee\": 3," +
		"\n\t\t\t\t\"expiry\": 4\n\t\t\t},\n\t\t\t" +
		"{\n\t\t\t\t\"chan_id\": 5," +
		"\n\t\t\t\t\"chan_capacity\": 6," +
		"\n\t\t\t\t\"amt_to_forward\": 7," +
		"\n\t\t\t\t\"fee\": 8," +
		"\n\t\t\t\t\"expiry\": 9\n\t\t\t}\n\t\t]\n\t}\n}\n"
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

// TestCommandWithTimeout calls the specified command with the specified LightningClient and args.
// Replaces stdout as the writer so that the output can be unit tested (without IO).
// Applies a timeout to the command call to prevent infinite looping
// since unit tests should terminate, even if non-termination would indicate a bug.
func RunCommandWithTimeout(
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
	case <-time.After(commandTimeout):
		// The command was blocking (probably) indefinitely,
		// which it is currently intended only if no EOF nor error occurred.
		return "", ErrTimeout
	}
}

func RunCommand(
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
	app.Run(args)

	return writer.Join(), err
}

// Verify that the expected output and command request are created
// for the specified command line args.
func TestCommandNoError(
	t *testing.T,
	command func(lnrpc.LightningClient, []string) (string, error),
	args []string,
	expectedRequest interface{},
	expectedResponseText string) {

	client := lnrpctesting.NewStubLightningClient()
	resp, err := command(&client, args)
	require.NoError(t, err)
	require.Equal(t, expectedResponseText, resp)

	require.Equal(t, expectedRequest, client.CapturedRequest,
		"Incorrect request passed to RPC.")
}

// Verify that the specified text is contained in the command output.
func TestCommandTextInResponse(
	t *testing.T,
	command func(lnrpc.LightningClient, []string) (string, error),
	args []string,
	expectedText string) {

	client := lnrpctesting.NewStubLightningClient()
	resp, err := command(&client, args)
	require.NoError(t, err)
	require.True(t, strings.Contains(resp, expectedText),
		fmt.Sprintf("Expected text %s to be present, but it wasn't contained in \n%s\n",
			expectedText, resp))
}

// Verify that the specified validation error occurs.
func TestCommandValidationError(
	t *testing.T,
	command func(lnrpc.LightningClient, []string) (string, error),
	args []string,
	expectedError error) {

	client := lnrpctesting.NewStubLightningClient()
	_, err := command(&client, args)

	require.Error(t, err)
	require.Equal(t, expectedError, err, "Unexpected error returned.")
}

// Verify that the specified text is present in the validation
// error that occurs.
// Useful for when there is variable or unwieldy text in the error.
func TestCommandTextInValidationError(
	t *testing.T,
	command func(lnrpc.LightningClient, []string) (string, error),
	args []string,
	expectedText string) {

	client := lnrpctesting.NewStubLightningClient()
	_, err := command(&client, args)
	require.Error(t, err)
	require.True(t,
		strings.Contains(err.Error(), expectedText),
		fmt.Sprintf("Expected text %s to be present, but it wasn't contained in \n%s\n",
			expectedText, err.Error()))
}

// Simulate an RPC failure then check that the expected error is returned.
func TestCommandRPCError(
	t *testing.T,
	command func(lnrpc.LightningClient, []string) (string, error),
	args []string,
	rpcError error,
	resultError error) {

	client := lnrpctesting.NewFailingStubLightningClient(rpcError)
	_, err := command(&client, args)

	require.Error(t, err)
	require.Equal(t, resultError, err, "Unexpected error returned.")
}

func NewSendPaymentLightningClient(stream *SendPaymentStream) lnrpctesting.StubLightningClient {
	client := lnrpctesting.NewStubLightningClient()
	clientStream := lnrpctesting.StubClientStream{stream}
	client.SendPaymentClient = lnrpctesting.StubSendPaymentClient{&clientStream}
	return client
}

// A Stream that allows setting errors on send and recv,
// or returning a specified SendResponse
// while capturing the SendRequest that is received so it can be verified in tests.
type SendPaymentStream struct {
	SendError           error
	RecvError           error
	SendResponse        *lnrpc.SendResponse
	CapturedSendRequest *lnrpc.SendRequest
}

func NewSendPaymentStream(sendError error, recvError error) SendPaymentStream {
	hop1 := lnrpc.Hop{0, 1, 2, 3, 4}
	hop2 := lnrpc.Hop{5, 6, 7, 8, 9}
	hops := []*lnrpc.Hop{&hop1, &hop2}
	route := lnrpc.Route{2, 5, 8, hops}
	sendResponse := lnrpc.SendResponse{"ERROR", []byte{12, 34, 56, 78}, &route}
	return SendPaymentStream{sendError, recvError, &sendResponse, nil}
}

func (s *SendPaymentStream) Context() context.Context {
	return new(lnrpctesting.StubContext)
}

func (s *SendPaymentStream) SendMsg(m interface{}) error {
	result, ok := m.(*lnrpc.SendRequest)
	if ok {
		s.CapturedSendRequest = result
	}

	return s.SendError
}

func (s *SendPaymentStream) RecvMsg(m interface{}) error {
	result, ok := m.(*lnrpc.SendResponse)

	if ok {
		result.PaymentError = s.SendResponse.PaymentError
		result.PaymentPreimage = s.SendResponse.PaymentPreimage
		result.PaymentRoute = s.SendResponse.PaymentRoute
	}

	return s.RecvError
}
