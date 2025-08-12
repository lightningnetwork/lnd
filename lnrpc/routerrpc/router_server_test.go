package routerrpc

import (
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/queue"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/zpay32"
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
	towerMock.queue.ChanIn() <- &paymentsdb.MPPayment{
		Info:   &paymentsdb.PaymentCreationInfo{},
		Status: paymentsdb.StatusInFlight,
	}
	towerMock.queue.ChanIn() <- &paymentsdb.MPPayment{
		Info:   &paymentsdb.PaymentCreationInfo{},
		Status: paymentsdb.StatusSucceeded,
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
	towerMock.queue.ChanIn() <- &paymentsdb.MPPayment{
		Info:   &paymentsdb.PaymentCreationInfo{},
		Status: paymentsdb.StatusInFlight,
	}
	towerMock.queue.ChanIn() <- &paymentsdb.MPPayment{
		Info:   &paymentsdb.PaymentCreationInfo{},
		Status: paymentsdb.StatusSucceeded,
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

// TestIsLsp tests the isLSP heuristic. Combinations of different route hints
// with different fees and cltv deltas are tested to ensure that the heuristic
// correctly identifies whether a route leads to an LSP or not.
func TestIsLsp(t *testing.T) {
	probeAmtMsat := lnwire.MilliSatoshi(1_000_000)

	alicePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	alicePubKey := alicePrivKey.PubKey()

	bobPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	bobPubKey := bobPrivKey.PubKey()

	carolPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	carolPubKey := carolPrivKey.PubKey()

	davePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	davePubKey := davePrivKey.PubKey()

	var (
		aliceHopHint = zpay32.HopHint{
			NodeID:                    alicePubKey,
			FeeBaseMSat:               100,
			FeeProportionalMillionths: 1_000,
			ChannelID:                 421337,
		}

		bobHopHint = zpay32.HopHint{
			NodeID:                    bobPubKey,
			FeeBaseMSat:               2_000,
			FeeProportionalMillionths: 2_000,
			CLTVExpiryDelta:           288,
			ChannelID:                 815,
		}

		carolHopHint = zpay32.HopHint{
			NodeID:                    carolPubKey,
			FeeBaseMSat:               2_000,
			FeeProportionalMillionths: 2_000,
			ChannelID:                 815,
		}

		daveHopHint = zpay32.HopHint{
			NodeID:                    davePubKey,
			FeeBaseMSat:               2_000,
			FeeProportionalMillionths: 2_000,
			ChannelID:                 815,
		}

		publicChannelID       = uint64(42)
		daveHopHintPublicChan = zpay32.HopHint{
			NodeID:                    davePubKey,
			FeeBaseMSat:               2_000,
			FeeProportionalMillionths: 2_000,
			ChannelID:                 publicChannelID,
		}
	)

	bobExpensiveCopy := bobHopHint.Copy()
	bobExpensiveCopy.FeeBaseMSat = 1_000_000
	bobExpensiveCopy.FeeProportionalMillionths = 1_000_000
	bobExpensiveCopy.CLTVExpiryDelta = bobHopHint.CLTVExpiryDelta - 1

	//nolint:ll
	lspTestCases := []struct {
		name           string
		routeHints     [][]zpay32.HopHint
		probeAmtMsat   lnwire.MilliSatoshi
		isLsp          bool
		expectedHints  [][]zpay32.HopHint
		expectedLspHop *zpay32.HopHint
	}{
		{
			name:           "empty route hints",
			routeHints:     [][]zpay32.HopHint{{}},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          false,
			expectedHints:  [][]zpay32.HopHint{},
			expectedLspHop: nil,
		},
		{
			name:           "single route hint",
			routeHints:     [][]zpay32.HopHint{{daveHopHint}},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          true,
			expectedHints:  [][]zpay32.HopHint{},
			expectedLspHop: &daveHopHint,
		},
		{
			name: "single route, multiple hints",
			routeHints: [][]zpay32.HopHint{{
				aliceHopHint, bobHopHint,
			}},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          true,
			expectedHints:  [][]zpay32.HopHint{{aliceHopHint}},
			expectedLspHop: &bobHopHint,
		},
		{
			name: "multiple routes, multiple hints",
			routeHints: [][]zpay32.HopHint{
				{
					aliceHopHint, bobHopHint,
				},
				{
					carolHopHint, bobHopHint,
				},
			},
			probeAmtMsat: probeAmtMsat,
			isLsp:        true,
			expectedHints: [][]zpay32.HopHint{
				{aliceHopHint}, {carolHopHint},
			},
			expectedLspHop: &bobHopHint,
		},
		{
			name: "multiple routes, multiple hints with min length",
			routeHints: [][]zpay32.HopHint{
				{
					bobHopHint,
				},
				{
					carolHopHint, bobHopHint,
				},
			},
			probeAmtMsat: probeAmtMsat,
			isLsp:        true,
			expectedHints: [][]zpay32.HopHint{
				{carolHopHint},
			},
			expectedLspHop: &bobHopHint,
		},
		{
			name: "multiple routes, multiple hints, diff fees+cltv",
			routeHints: [][]zpay32.HopHint{
				{
					bobHopHint,
				},
				{
					carolHopHint, bobExpensiveCopy,
				},
			},
			probeAmtMsat: probeAmtMsat,
			isLsp:        true,
			expectedHints: [][]zpay32.HopHint{
				{carolHopHint},
			},
			expectedLspHop: &zpay32.HopHint{
				NodeID:                    bobHopHint.NodeID,
				ChannelID:                 bobHopHint.ChannelID,
				FeeBaseMSat:               bobExpensiveCopy.FeeBaseMSat,
				FeeProportionalMillionths: bobExpensiveCopy.FeeProportionalMillionths,
				CLTVExpiryDelta:           bobHopHint.CLTVExpiryDelta,
			},
		},
		{
			name: "multiple routes, different final hops",
			routeHints: [][]zpay32.HopHint{
				{
					aliceHopHint, bobHopHint,
				},
				{
					carolHopHint, daveHopHint,
				},
			},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          false,
			expectedHints:  [][]zpay32.HopHint{},
			expectedLspHop: nil,
		},
		{
			name: "multiple routes, same public hops",
			routeHints: [][]zpay32.HopHint{
				{
					aliceHopHint, daveHopHintPublicChan,
				},
				{
					carolHopHint, daveHopHintPublicChan,
				},
			},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          false,
			expectedHints:  [][]zpay32.HopHint{},
			expectedLspHop: nil,
		},
		{
			name: "multiple routes, same public hops",
			routeHints: [][]zpay32.HopHint{
				{
					aliceHopHint, daveHopHint,
				},
				{
					carolHopHint, daveHopHintPublicChan,
				},
				{
					aliceHopHint, daveHopHintPublicChan,
				},
			},
			probeAmtMsat:   probeAmtMsat,
			isLsp:          false,
			expectedHints:  [][]zpay32.HopHint{},
			expectedLspHop: nil,
		},
	}

	// Returns ErrEdgeNotFound for private channels.
	fetchChannelEndpoints := func(chanID uint64) (route.Vertex,
		route.Vertex, error) {

		if chanID == publicChannelID {
			return route.Vertex{}, route.Vertex{}, nil
		}

		return route.Vertex{}, route.Vertex{}, graphdb.ErrEdgeNotFound
	}

	for _, tc := range lspTestCases {
		t.Run(tc.name, func(t *testing.T) {
			isLsp := isLSP(tc.routeHints, fetchChannelEndpoints)
			require.Equal(t, tc.isLsp, isLsp)
			if !tc.isLsp {
				return
			}

			adjustedHints, lspHint, _ := prepareLspRouteHints(
				tc.routeHints, tc.probeAmtMsat,
			)
			require.Equal(t, tc.expectedHints, adjustedHints)
			require.Equal(t, tc.expectedLspHop, lspHint)
		})
	}
}
