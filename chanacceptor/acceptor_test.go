package chanacceptor

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testTimeout = time.Second

type channelAcceptorCtx struct {
	t *testing.T

	// extRequests is the channel that we send our channel accept requests
	// into, this channel mocks sending of a request to the rpc acceptor.
	// This channel should be buffered with the number of requests we want
	// to send so that it does not block (like a rpc stream).
	extRequests chan []byte

	// responses is a map of pending channel IDs to the response which we
	// wish to mock the remote channel acceptor sending.
	responses map[[32]byte]*lnrpc.ChannelAcceptResponse

	// acceptor is the channel acceptor we create for the test.
	acceptor *RPCAcceptor

	// errChan is a channel that the error the channel acceptor exits with
	// is sent into.
	errChan chan error

	// quit is a channel that can be used to shutdown the channel acceptor
	// and return errShuttingDown.
	quit chan struct{}
}

func newChanAcceptorCtx(t *testing.T, acceptCallCount int,
	responses map[[32]byte]*lnrpc.ChannelAcceptResponse) *channelAcceptorCtx {

	testCtx := &channelAcceptorCtx{
		t:           t,
		extRequests: make(chan []byte, acceptCallCount),
		responses:   responses,
		errChan:     make(chan error),
		quit:        make(chan struct{}),
	}

	testCtx.acceptor = NewRPCAcceptor(
		testCtx.receiveResponse, testCtx.sendRequest, testTimeout*5,
		&chaincfg.RegressionNetParams, testCtx.quit,
	)

	return testCtx
}

// sendRequest mocks sending a request to the channel acceptor.
func (c *channelAcceptorCtx) sendRequest(request *lnrpc.ChannelAcceptRequest) error {
	select {
	case c.extRequests <- request.PendingChanId:

	case <-time.After(testTimeout):
		c.t.Fatalf("timeout sending request: %v", request.PendingChanId)
	}

	return nil
}

// receiveResponse mocks sending of a response from the channel acceptor.
func (c *channelAcceptorCtx) receiveResponse() (*lnrpc.ChannelAcceptResponse,
	error) {

	select {
	case id := <-c.extRequests:
		scratch := [32]byte{}
		copy(scratch[:], id)

		resp, ok := c.responses[scratch]
		assert.True(c.t, ok)

		return resp, nil

	case <-time.After(testTimeout):
		c.t.Fatalf("timeout receiving request")
		return nil, errors.New("receiveResponse timeout")

	// Exit if our test acceptor closes the done channel, which indicates
	// that the acceptor is shutting down.
	case <-c.acceptor.done:
		return nil, errors.New("acceptor shutting down")
	}
}

// start runs our channel acceptor in a goroutine which sends its exit error
// into our test error channel.
func (c *channelAcceptorCtx) start() {
	go func() {
		c.errChan <- c.acceptor.Run()
	}()
}

// stop shuts down the test's channel acceptor and asserts that it exits with
// our expected error.
func (c *channelAcceptorCtx) stop() {
	close(c.quit)

	select {
	case actual := <-c.errChan:
		assert.Equal(c.t, errShuttingDown, actual)

	case <-time.After(testTimeout):
		c.t.Fatal("timeout waiting for acceptor to exit")
	}
}

// queryAndAssert takes a map of open channel requests which we want to call
// Accept for to the outcome we expect from the acceptor, dispatches each
// request in a goroutine and then asserts that we get the outcome we expect.
func (c *channelAcceptorCtx) queryAndAssert(queries map[*lnwire.OpenChannel]*ChannelAcceptResponse) {
	var (
		node = btcec.NewPublicKey(
			new(btcec.FieldVal).SetInt(1),
			new(btcec.FieldVal).SetInt(1),
		)

		responses = make(chan struct{})
	)

	for request, expected := range queries {
		request := request
		expected := expected

		go func() {
			resp := c.acceptor.Accept(&ChannelAcceptRequest{
				Node:        node,
				OpenChanMsg: request,
			})
			assert.Equal(c.t, expected, resp)
			responses <- struct{}{}
		}()
	}

	// Wait for each of our requests to return a response before we exit.
	for i := 0; i < len(queries); i++ {
		select {
		case <-responses:
		case <-time.After(testTimeout):
			c.t.Fatalf("did not receive response")
		}
	}
}

// TestMultipleAcceptClients tests that the RPC acceptor is capable of handling
// multiple requests to its Accept function and responding to them correctly.
func TestMultipleAcceptClients(t *testing.T) {
	testAddr := "bcrt1qwrmq9uca0t3dy9t9wtuq5tm4405r7tfzyqn9pp"
	testUpfront, err := chancloser.ParseUpfrontShutdownAddress(
		testAddr, &chaincfg.RegressionNetParams,
	)
	require.NoError(t, err)

	var (
		chan1 = &lnwire.OpenChannel{
			PendingChannelID: [32]byte{1},
		}
		chan2 = &lnwire.OpenChannel{
			PendingChannelID: [32]byte{2},
		}
		chan3 = &lnwire.OpenChannel{
			PendingChannelID: [32]byte{3},
		}

		customError = errors.New("go away")

		// Queries is a map of the channel IDs we will query Accept
		// with, and the set of outcomes we expect.
		queries = map[*lnwire.OpenChannel]*ChannelAcceptResponse{
			chan1: NewChannelAcceptResponse(
				true, nil, testUpfront, 1, 2, 3, 4, 5, 6,
				false,
			),
			chan2: NewChannelAcceptResponse(
				false, errChannelRejected, nil, 0, 0, 0,
				0, 0, 0, false,
			),
			chan3: NewChannelAcceptResponse(
				false, customError, nil, 0, 0, 0, 0, 0, 0,
				false,
			),
		}

		// Responses is a mocked set of responses from the remote
		// channel acceptor.
		responses = map[[32]byte]*lnrpc.ChannelAcceptResponse{
			chan1.PendingChannelID: {
				PendingChanId:   chan1.PendingChannelID[:],
				Accept:          true,
				UpfrontShutdown: testAddr,
				CsvDelay:        1,
				MaxHtlcCount:    2,
				MinAcceptDepth:  3,
				ReserveSat:      4,
				InFlightMaxMsat: 5,
				MinHtlcIn:       6,
			},
			chan2.PendingChannelID: {
				PendingChanId: chan2.PendingChannelID[:],
				Accept:        false,
			},
			chan3.PendingChannelID: {
				PendingChanId: chan3.PendingChannelID[:],
				Accept:        false,
				Error:         customError.Error(),
			},
		}
	)

	// Create and start our channel acceptor.
	testCtx := newChanAcceptorCtx(t, len(queries), responses)
	testCtx.start()

	// Dispatch three queries and assert that we get our expected response.
	// for each.
	testCtx.queryAndAssert(queries)

	// Shutdown our acceptor.
	testCtx.stop()
}

// TestInvalidResponse tests the case where our remote channel acceptor sends us
// an invalid response, so the channel acceptor stream terminates.
func TestInvalidResponse(t *testing.T) {
	var (
		chan1 = [32]byte{1}

		// We make a single query, and expect it to fail with our
		// generic error because our response is invalid.
		queries = map[*lnwire.OpenChannel]*ChannelAcceptResponse{
			{
				PendingChannelID: chan1,
			}: NewChannelAcceptResponse(
				false, errChannelRejected, nil, 0, 0,
				0, 0, 0, 0, false,
			),
		}

		// Create a single response which is invalid because it accepts
		// the channel but also contains an error message.
		responses = map[[32]byte]*lnrpc.ChannelAcceptResponse{
			chan1: {
				PendingChanId: chan1[:],
				Accept:        true,
				Error:         "has an error as well",
			},
		}
	)

	// Create and start our channel acceptor.
	testCtx := newChanAcceptorCtx(t, len(queries), responses)
	testCtx.start()

	testCtx.queryAndAssert(queries)

	// We do not expect our channel acceptor to exit because of one invalid
	// response, so we shutdown and assert here.
	testCtx.stop()
}

// TestInvalidReserve tests validation of the channel reserve proposed by the
// acceptor against the dust limit that was proposed by the remote peer.
func TestInvalidReserve(t *testing.T) {
	var (
		chan1 = [32]byte{1}

		dustLimit = btcutil.Amount(1000)
		reserve   = dustLimit / 2

		// We make a single query, and expect it to fail with our
		// generic error because channel reserve is too low.
		queries = map[*lnwire.OpenChannel]*ChannelAcceptResponse{
			{
				PendingChannelID: chan1,
				DustLimit:        dustLimit,
			}: NewChannelAcceptResponse(
				false, errChannelRejected, nil, 0, 0,
				0, reserve, 0, 0, false,
			),
		}

		// Create a single response which is invalid because the
		// proposed reserve is below our dust limit.
		responses = map[[32]byte]*lnrpc.ChannelAcceptResponse{
			chan1: {
				PendingChanId: chan1[:],
				Accept:        true,
				ReserveSat:    uint64(reserve),
			},
		}
	)

	// Create and start our channel acceptor.
	testCtx := newChanAcceptorCtx(t, len(queries), responses)
	testCtx.start()

	testCtx.queryAndAssert(queries)

	// We do not expect our channel acceptor to exit because of one invalid
	// response, so we shutdown and assert here.
	testCtx.stop()
}
