package routerrpc

import (
	"context"
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/subscribe"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

type streamPaymentsMock struct {
	grpc.ServerStream
	ctx            context.Context
	sentFromServer chan *lnrpc.Payment
}

func makeStreamPaymentMock(ctx context.Context) *streamPaymentsMock {
	return &streamPaymentsMock{
		ctx:            ctx,
		sentFromServer: make(chan *lnrpc.Payment, 10),
	}
}

func (m *streamPaymentsMock) Context() context.Context {
	return m.ctx
}

func (m *streamPaymentsMock) Send(p *lnrpc.Payment) error {
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
	stream := makeStreamPaymentMock(streamCtx)

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				Tower: towerMock,
			},
		},
	}

	// Cancel stream immediately.
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
	stream := makeStreamPaymentMock(streamCtx)
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
	stream := makeStreamPaymentMock(streamCtx)
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

type streamHtlcEventsMock struct {
	grpc.ServerStream
	ctx            context.Context //nolint
	sentFromServer chan *HtlcEvent
}

func (m *streamHtlcEventsMock) Send(p *HtlcEvent) error {
	m.sentFromServer <- p
	return nil
}

func (m *streamHtlcEventsMock) Context() context.Context {
	return m.ctx
}

func makeStreamHtlcEventsMock(ctx context.Context) *streamHtlcEventsMock {
	return &streamHtlcEventsMock{
		ctx:            ctx,
		sentFromServer: make(chan *HtlcEvent, 20),
	}
}

// TestSubscribeHtlcEventsReturnsOnCancelContext tests whether
// SubscribeHtlcEvents returns when the stream context is cancelled.
func TestSubscribeHtlcEventsReturnsOnCancelContext(t *testing.T) {
	t.Parallel()
	request := &SubscribeHtlcEventsRequest{}

	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream := makeStreamHtlcEventsMock(streamCtx)

	// Create server mock for htlc propagation.
	htlcServerMock := subscribe.NewServer()
	err := htlcServerMock.Start()
	if err != nil {
		t.Fatalf("starting htlc server failed with: %s", err)
	}
	defer func() {
		err := htlcServerMock.Stop()
		if err != nil {
			t.Fatalf("failed stopping htlc server: %s", err)
		}
	}()

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				SubscribeHtlcEvents: func() (
					*subscribe.Client, error) {

					return htlcServerMock.Subscribe()
				},
			},
		},
	}

	// Cancel stream immediately.
	cancelStream()

	// Make sure the call returns.
	err = server.SubscribeHtlcEvents(request, stream)
	require.Equal(t, context.Canceled, err)
}

// TestHtlcEvents tests whether all htlc events are propagated to the client.
func TestSubscribeHtlcEvents(t *testing.T) {
	t.Parallel()
	request := &SubscribeHtlcEventsRequest{}

	// Create server mock for htlc propagation.
	htlcNotifierMock := subscribe.NewServer()
	err := htlcNotifierMock.Start()
	if err != nil {
		t.Fatalf("starting htlc server failed with: %s", err)
	}
	defer func() {
		err := htlcNotifierMock.Stop()
		if err != nil {
			t.Fatalf("failed stopping htlc server: %s", err)
		}
	}()

	streamCtx, cancelStream := context.WithCancel(context.Background())
	stream := makeStreamHtlcEventsMock(streamCtx)
	defer cancelStream()

	server := &Server{
		cfg: &Config{
			RouterBackend: &RouterBackend{
				SubscribeHtlcEvents: func() (
					*subscribe.Client, error) {

					return htlcNotifierMock.Subscribe()
				},
			},
		},
	}

	// Listen to htlc events in a goroutine.
	go func() {
		err := server.SubscribeHtlcEvents(request, stream)
		require.Equal(t, context.Canceled, err)
	}()

	// Make sure the client is subscribed before we send htlc events.
	time.Sleep(50 * time.Millisecond)

	// Send all types of HTLC events through the server and see whether
	// the client receives the right types in right order.
	err = htlcNotifierMock.SendUpdate(&htlcswitch.ForwardingFailEvent{})
	if err != nil {
		t.Fatalf("failed sending ForwardFailEvent to the "+
			"htlcNotifier: %s", err)
	}
	err = htlcNotifierMock.SendUpdate(&htlcswitch.SettleEvent{})
	if err != nil {
		t.Fatalf("failed sending SettleEvent to the "+
			"htlcNotifier: %s", err)
	}
	err = htlcNotifierMock.SendUpdate(&htlcswitch.FinalHtlcEvent{})
	if err != nil {
		t.Fatalf("failed sending FinalHtlcEvent to the "+
			"htlcNotifier: %s", err)
	}
	// Make sure the LinkError is populated otherwise the test fails.
	err = htlcNotifierMock.SendUpdate(&htlcswitch.LinkFailEvent{
		LinkError: htlcswitch.NewLinkError(
			lnwire.NewFeeInsufficient(0, lnwire.ChannelUpdate{}),
		),
	})
	if err != nil {
		t.Fatalf("failed sending LinkFailEvent to the "+
			"htlcNotifier: %s", err)
	}

	// Wait until there's 5 update or the deadline is exceeded.
	deadline := time.Now().Add(1 * time.Second)
	for {
		if len(stream.sentFromServer) == 5 {
			break
		}

		if time.Now().After(deadline) {
			require.FailNow(t, "deadline exceeded.")
		}
	}

	// 5 updates should be sent to the client. The server sends one initial
	//  subscribe request we need to account for.
	require.Len(t, stream.sentFromServer, 5)

	htlcEvent := <-stream.sentFromServer
	_, ok := htlcEvent.Event.(*HtlcEvent_SubscribedEvent)
	require.True(t, ok)

	htlcEvent = <-stream.sentFromServer
	_, ok = htlcEvent.Event.(*HtlcEvent_ForwardFailEvent)
	require.True(t, ok)

	htlcEvent = <-stream.sentFromServer
	_, ok = htlcEvent.Event.(*HtlcEvent_SettleEvent)
	require.True(t, ok)

	htlcEvent = <-stream.sentFromServer
	_, ok = htlcEvent.Event.(*HtlcEvent_FinalHtlcEvent)
	require.True(t, ok)

	htlcEvent = <-stream.sentFromServer
	_, ok = htlcEvent.Event.(*HtlcEvent_LinkFailEvent)
	require.True(t, ok)
}
