package routerrpc

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type streamMock struct {
	grpc.ServerStream
	ctx            context.Context
	sentFromServer chan *lnrpc.Payment
}

func makeStreamMock(ctx context.Context) *streamMock {
	return &streamMock{
		ctx:            ctx,
		sentFromServer: make(chan *lnrpc.Payment, 10),
	}
}

func (m *streamMock) Context() context.Context {
	return m.ctx
}

func (m *streamMock) Send(p *lnrpc.Payment) error {
	m.sentFromServer <- p
	return nil
}

type controlTowerSubscriberMock struct {
	updates <-chan interface{}
}

func (s controlTowerSubscriberMock) Updates() <-chan interface{} {
	return s.updates
}

func (s controlTowerSubscriberMock) Close() {
}

type controlTowerMock struct {
	queue *queue.ConcurrentQueue
	routing.ControlTower
}

func makeControlTowerMock() *controlTowerMock {
	towerMock := &controlTowerMock{
		queue: queue.NewConcurrentQueue(20),
	}
	towerMock.queue.Start()

	return towerMock
}

func (t *controlTowerMock) SubscribeAllPayments() (
	routing.ControlTowerSubscriber, error) {

	return &controlTowerSubscriberMock{
		updates: t.queue.ChanOut(),
	}, nil
}

// TestTrackPaymentsReturnsOnCancelContext tests whether TrackPayments returns
// when the stream context is cancelled.
func TestTrackPaymentsReturnsOnCancelContext(t *testing.T) {
	// Setup mocks and request.
	request := &TrackPaymentsRequest{
		NoInflightUpdates: false,
	}
	towerMock := makeControlTowerMock()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream := makeStreamMock(streamCtx)

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				Tower: towerMock,
			},
		},
	}

	// Cancel stream immediately
	cancelStream()

	// Make sure the call returns.
	err := server.TrackPayments(request, stream)
	require.Equal(t, context.Canceled, err)
}

// TestTrackPaymentsInflightUpdate tests whether all updates from the control
// tower are propagated to the client.
func TestTrackPaymentsInflightUpdates(t *testing.T) {
	// Setup mocks and request.
	request := &TrackPaymentsRequest{
		NoInflightUpdates: false,
	}
	towerMock := makeControlTowerMock()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream := makeStreamMock(streamCtx)
	defer cancelStream()

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				Tower: towerMock,
			},
		},
	}

	// Listen to payment updates in a goroutine.
	go func() {
		err := server.TrackPayments(request, stream)
		require.Equal(t, context.Canceled, err)
	}()

	// Enqueue some payment updates on the mock.
	towerMock.queue.ChanIn() <- &channeldb.MPPayment{
		Info:   &channeldb.PaymentCreationInfo{},
		Status: channeldb.StatusInFlight,
	}
	towerMock.queue.ChanIn() <- &channeldb.MPPayment{
		Info:   &channeldb.PaymentCreationInfo{},
		Status: channeldb.StatusSucceeded,
	}

	// Wait until there's 2 updates or the deadline is exceeded.
	deadline := time.Now().Add(1 * time.Second)
	for {
		if len(stream.sentFromServer) == 2 {
			break
		}

		if time.Now().After(deadline) {
			require.FailNow(t, "deadline exceeded.")
		}
	}

	// Both updates should be sent to the client.
	require.Len(t, stream.sentFromServer, 2)

	// The updates should be in the right order.
	payment := <-stream.sentFromServer
	require.Equal(t, lnrpc.Payment_IN_FLIGHT, payment.Status)
	payment = <-stream.sentFromServer
	require.Equal(t, lnrpc.Payment_SUCCEEDED, payment.Status)
}

// TestTrackPaymentsInflightUpdate tests whether only final updates from the
// control tower are propagated to the client when noInflightUpdates = true.
func TestTrackPaymentsNoInflightUpdates(t *testing.T) {
	// Setup mocks and request.
	request := &TrackPaymentsRequest{
		NoInflightUpdates: true,
	}
	towerMock := &controlTowerMock{
		queue: queue.NewConcurrentQueue(20),
	}
	towerMock.queue.Start()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream := makeStreamMock(streamCtx)
	defer cancelStream()

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				Tower: towerMock,
			},
		},
	}

	// Listen to payment updates in a goroutine.
	go func() {
		err := server.TrackPayments(request, stream)
		require.Equal(t, context.Canceled, err)
	}()

	// Enqueue some payment updates on the mock.
	towerMock.queue.ChanIn() <- &channeldb.MPPayment{
		Info:   &channeldb.PaymentCreationInfo{},
		Status: channeldb.StatusInFlight,
	}
	towerMock.queue.ChanIn() <- &channeldb.MPPayment{
		Info:   &channeldb.PaymentCreationInfo{},
		Status: channeldb.StatusSucceeded,
	}

	// Wait until there's 1 update or the deadline is exceeded.
	deadline := time.Now().Add(1 * time.Second)
	for {
		if len(stream.sentFromServer) == 1 {
			break
		}

		if time.Now().After(deadline) {
			require.FailNow(t, "deadline exceeded.")
		}
	}

	// Only 1 update should be sent to the client.
	require.Len(t, stream.sentFromServer, 1)

	// Only the final states should be sent to the client.
	payment := <-stream.sentFromServer
	require.Equal(t, lnrpc.Payment_SUCCEEDED, payment.Status)
}
