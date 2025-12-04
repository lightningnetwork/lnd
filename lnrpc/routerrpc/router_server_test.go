package routerrpc

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
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

	streamCtx, cancelStream := context.WithCancel(t.Context())
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

	streamCtx, cancelStream := context.WithCancel(t.Context())
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

	streamCtx, cancelStream := context.WithCancel(t.Context())
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

// TestIsLsp tests the isLSP heuristic. It validates all three LSP detection
// rules:
// Rule 1: Invoice target is public => not LSP.
// Rule 2: All destination hop hints are private => not LSP (Boltz case).
// Rule 3: At least one destination hop hint is public => LSP (Muun case).
func TestIsLsp(t *testing.T) {
	// Setup test nodes:
	// - Alice: public node (in graph)
	// - Bob: private node
	// - Carol: private node
	// - Dave: public node (in graph)
	// - Eve: private node
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

	evePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	evePubKey := evePrivKey.PubKey()

	// Create hop hints for each node.
	aliceHopHint := zpay32.HopHint{
		NodeID:                    alicePubKey,
		FeeBaseMSat:               100,
		FeeProportionalMillionths: 1_000,
		CLTVExpiryDelta:           40,
		ChannelID:                 1,
	}

	bobHopHint := zpay32.HopHint{
		NodeID:                    bobPubKey,
		FeeBaseMSat:               2_000,
		FeeProportionalMillionths: 2_000,
		CLTVExpiryDelta:           144,
		ChannelID:                 2,
	}

	carolHopHint := zpay32.HopHint{
		NodeID:                    carolPubKey,
		FeeBaseMSat:               1_500,
		FeeProportionalMillionths: 1_500,
		CLTVExpiryDelta:           144,
		ChannelID:                 3,
	}

	daveHopHint := zpay32.HopHint{
		NodeID:                    davePubKey,
		FeeBaseMSat:               3_000,
		FeeProportionalMillionths: 3_000,
		CLTVExpiryDelta:           288,
		ChannelID:                 4,
	}

	eveHopHint := zpay32.HopHint{
		NodeID:                    evePubKey,
		FeeBaseMSat:               500,
		FeeProportionalMillionths: 500,
		CLTVExpiryDelta:           40,
		ChannelID:                 5,
	}

	// Mock hasNode: returns true only for alice and dave.
	hasNode := func(nodePub route.Vertex) (bool, error) {
		aliceVertex := route.NewVertex(alicePubKey)
		daveVertex := route.NewVertex(davePubKey)
		return bytes.Equal(nodePub[:], aliceVertex[:]) ||
			bytes.Equal(nodePub[:], daveVertex[:]), nil
	}

	tests := []struct {
		name          string
		routeHints    [][]zpay32.HopHint
		invoiceTarget []byte
		expectLSP     bool
	}{
		// Edge cases.
		{
			name:          "no route hints",
			routeHints:    [][]zpay32.HopHint{},
			invoiceTarget: nil,
			expectLSP:     false,
		},
		{
			name:          "empty route hint array",
			routeHints:    [][]zpay32.HopHint{{}},
			invoiceTarget: nil,
			expectLSP:     false,
		},

		// Rule 1: Invoice target is public => NOT an LSP.
		// Rationale: Can route directly to public target.
		{
			name: "invoice target is public (alice)",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, carolHopHint},
			},
			invoiceTarget: alicePubKey.SerializeCompressed(),
			expectLSP:     false,
		},
		{
			name: "invoice target is public with public dest hop",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, daveHopHint},
			},
			invoiceTarget: davePubKey.SerializeCompressed(),
			expectLSP:     false,
		},
		{
			name: "invoice target is public with multiple routes",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, carolHopHint},
				{aliceHopHint, daveHopHint},
			},
			invoiceTarget: alicePubKey.SerializeCompressed(),
			expectLSP:     false,
		},

		// Rule 2: All destination hop hints are private => NOT an LSP.
		// Rationale: The destination hop hint is private so it cannot
		// be probed so we default to NOT an LSP.
		{
			name: "single route to private dest",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint, bobHopHint},
			},
			invoiceTarget: bobPubKey.SerializeCompressed(),
			expectLSP:     false,
		},
		{
			name: "multiple routes, all to private dests",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint, bobHopHint},
				{daveHopHint, carolHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     false,
		},
		{
			name: "single hop to private node",
			routeHints: [][]zpay32.HopHint{
				{eveHopHint},
			},
			invoiceTarget: evePubKey.SerializeCompressed(),
			expectLSP:     false,
		},
		{
			name: "all routes to same private node",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint, bobHopHint},
				{daveHopHint, bobHopHint},
				{carolHopHint, bobHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     false,
		},

		// Rule 3: At least one destination hop is public => IS an LSP.
		// Rationale: As long as there is at least one public
		// destination route hint, it is an LSP setup and can be probed.
		{
			name: "single route to public dest (dave)",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, daveHopHint},
			},
			invoiceTarget: evePubKey.SerializeCompressed(),
			expectLSP:     true,
		},
		{
			name: "direct hop to public LSP (alice)",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint},
			},
			invoiceTarget: bobPubKey.SerializeCompressed(),
			expectLSP:     true,
		},
		{
			name: "multiple routes to same public LSP (dave)",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, daveHopHint},
				{carolHopHint, daveHopHint},
				{eveHopHint, daveHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     true,
		},
		{
			name: "multiple routes to different public LSPs",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint},
				{carolHopHint, daveHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     true,
		},
		{
			name: "mixed public and private dest hops",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint, bobHopHint},
				{carolHopHint, daveHopHint},
				{bobHopHint, eveHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     true,
		},
		{
			name: "first route has public dest, rest private",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint},
				{carolHopHint, eveHopHint},
			},
			invoiceTarget: nil,
			expectLSP:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isLSP(
				tt.routeHints, tt.invoiceTarget, hasNode,
			)
			require.Equal(t, tt.expectLSP, result)
		})
	}
}

// TestPrepareLspRouteHints tests the prepareLspRouteHints function to ensure
// it correctly filters, groups, and calculates worst-case fees for LSP routes.
func TestPrepareLspRouteHints(t *testing.T) {
	// Setup test nodes:
	// - Alice: public LSP node (in graph)
	// - Bob: private node
	// - Carol: private node
	// - Dave: public LSP node (in graph)
	// - Eve: private node
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

	evePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	evePubKey := evePrivKey.PubKey()

	// Create hop hints with varying fees and CLTV deltas.
	aliceHopHint1 := zpay32.HopHint{
		NodeID:                    alicePubKey,
		FeeBaseMSat:               100,
		FeeProportionalMillionths: 1_000,
		CLTVExpiryDelta:           40,
		ChannelID:                 1,
	}

	aliceHopHint2 := zpay32.HopHint{
		NodeID:                    alicePubKey,
		FeeBaseMSat:               200,
		FeeProportionalMillionths: 2_000,
		CLTVExpiryDelta:           80,
		ChannelID:                 2,
	}

	bobHopHint := zpay32.HopHint{
		NodeID:                    bobPubKey,
		FeeBaseMSat:               500,
		FeeProportionalMillionths: 500,
		CLTVExpiryDelta:           144,
		ChannelID:                 3,
	}

	carolHopHint := zpay32.HopHint{
		NodeID:                    carolPubKey,
		FeeBaseMSat:               300,
		FeeProportionalMillionths: 300,
		CLTVExpiryDelta:           40,
		ChannelID:                 4,
	}

	daveHopHint1 := zpay32.HopHint{
		NodeID:                    davePubKey,
		FeeBaseMSat:               1_000,
		FeeProportionalMillionths: 1_000,
		CLTVExpiryDelta:           144,
		ChannelID:                 5,
	}

	daveHopHint2 := zpay32.HopHint{
		NodeID:                    davePubKey,
		FeeBaseMSat:               2_000,
		FeeProportionalMillionths: 500,
		CLTVExpiryDelta:           288,
		ChannelID:                 6,
	}

	eveHopHint := zpay32.HopHint{
		NodeID:                    evePubKey,
		FeeBaseMSat:               100,
		FeeProportionalMillionths: 100,
		CLTVExpiryDelta:           40,
		ChannelID:                 7,
	}

	// Mock hasNode: returns true only for alice and dave.
	hasNode := func(nodePub route.Vertex) (bool, error) {
		aliceVertex := route.NewVertex(alicePubKey)
		daveVertex := route.NewVertex(davePubKey)
		return bytes.Equal(nodePub[:], aliceVertex[:]) ||
			bytes.Equal(nodePub[:], daveVertex[:]), nil
	}

	amt := lnwire.MilliSatoshi(1_000_000)

	tests := []struct {
		name         string
		routeHints   [][]zpay32.HopHint
		expectedGrps int
		validateFunc func(t *testing.T,
			groups map[route.Vertex]*LspRouteGroup)
	}{
		{
			name: "single public LSP with one route",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint1},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				require.Len(t, groups, 1)

				// Find alice's group.
				aliceKey := route.NewVertex(alicePubKey)
				group, ok := groups[aliceKey]
				require.True(t, ok, "alice group not found")

				// Verify LSP hop hint.
				require.Equal(t, aliceHopHint1.FeeBaseMSat,
					group.LspHopHint.FeeBaseMSat)
				require.Equal(t, aliceHopHint1.CLTVExpiryDelta,
					group.LspHopHint.CLTVExpiryDelta)

				// Verify adjusted route hints.
				require.Len(t, group.AdjustedRouteHints, 1)
				require.Len(t, group.AdjustedRouteHints[0], 1)
				require.Equal(t, bobHopHint.NodeID,
					group.AdjustedRouteHints[0][0].NodeID)
			},
		},
		{
			name: "single LSP with multiple routes, same fees",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint1},
				{carolHopHint, aliceHopHint1},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				aliceKey := route.NewVertex(alicePubKey)
				group, ok := groups[aliceKey]
				require.True(t, ok, "alice group not found")

				// Should have 2 adjusted route hints.
				require.Len(t, group.AdjustedRouteHints, 2)

				// Fees should match the single hop hint.
				require.Equal(t, aliceHopHint1.FeeBaseMSat,
					group.LspHopHint.FeeBaseMSat)
				require.Equal(t, aliceHopHint1.CLTVExpiryDelta,
					group.LspHopHint.CLTVExpiryDelta)
			},
		},
		{
			name: "single LSP with different fees, uses worst case",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint1},
				{carolHopHint, aliceHopHint2},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				aliceKey := route.NewVertex(alicePubKey)
				group, ok := groups[aliceKey]
				require.True(t, ok, "alice group not found")

				// Should use worst-case (higher) fees.
				fee1 := aliceHopHint1.HopFee(amt)
				fee2 := aliceHopHint2.HopFee(amt)
				require.Greater(t, fee2, fee1,
					"hint2 should have higher fees")

				// Group should have hint2's fees.
				require.Equal(t, aliceHopHint2.FeeBaseMSat,
					group.LspHopHint.FeeBaseMSat)

				//nolint:ll
				require.Equal(t,
					aliceHopHint2.FeeProportionalMillionths,
					group.LspHopHint.FeeProportionalMillionths)

				// Should use worst-case CLTV delta.
				require.Equal(t, aliceHopHint2.CLTVExpiryDelta,
					group.LspHopHint.CLTVExpiryDelta)
			},
		},
		{
			name: "multiple public LSPs",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, aliceHopHint1},
				{carolHopHint, daveHopHint1},
			},
			expectedGrps: 2,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				require.Len(t, groups, 2)

				aliceKey := route.NewVertex(alicePubKey)
				daveKey := route.NewVertex(davePubKey)

				_, hasAlice := groups[aliceKey]
				_, hasDave := groups[daveKey]
				require.True(t, hasAlice, "alice group missing")
				require.True(t, hasDave, "dave group missing")
			},
		},
		{
			name: "filters out private dest hops",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint1, bobHopHint},
				{carolHopHint, daveHopHint1},
				{bobHopHint, eveHopHint},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				require.Len(t, groups, 1)

				daveKey := route.NewVertex(davePubKey)
				group, ok := groups[daveKey]
				require.True(t, ok, "dave group not found")

				// Only one route hint should remain
				require.Len(t, group.AdjustedRouteHints, 1)
			},
		},
		{
			name: "multiple routes to same LSP with varying CLTV",
			routeHints: [][]zpay32.HopHint{
				{bobHopHint, daveHopHint1},
				{carolHopHint, daveHopHint2},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				daveKey := route.NewVertex(davePubKey)
				group, ok := groups[daveKey]
				require.True(t, ok, "dave group not found")

				// Should use maximum CLTV delta.
				require.Equal(t, daveHopHint2.CLTVExpiryDelta,
					group.LspHopHint.CLTVExpiryDelta)
			},
		},
		{
			name: "single hop to public LSP",
			routeHints: [][]zpay32.HopHint{
				{aliceHopHint1},
			},
			expectedGrps: 1,
			validateFunc: func(t *testing.T,
				groups map[route.Vertex]*LspRouteGroup) {

				aliceKey := route.NewVertex(alicePubKey)
				group, ok := groups[aliceKey]
				require.True(t, ok, "alice group not found")

				// No adjusted hints since it's a direct hop
				require.Len(t, group.AdjustedRouteHints, 0)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, err := prepareLspRouteHints(
				tt.routeHints, amt, hasNode,
			)
			require.NoError(t, err)
			require.Len(t, groups, tt.expectedGrps)

			// Run custom validation if provided.
			if tt.validateFunc != nil {
				tt.validateFunc(t, groups)
			}
		})
	}

	// Error cases which in operation should never happen because we always
	// call isLSP first to check if the route hints are an LSP setup.
	t.Run("error: no route hints", func(t *testing.T) {
		_, err := prepareLspRouteHints(
			[][]zpay32.HopHint{}, amt, hasNode,
		)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no route hints")
	})

	t.Run("error: no public LSP nodes found", func(t *testing.T) {
		// All private destination hops. If all destination hops are
		// private we cannot probe any LSPs so we return an error.
		routeHints := [][]zpay32.HopHint{
			{aliceHopHint1, bobHopHint},
			{daveHopHint1, carolHopHint},
		}
		_, err := prepareLspRouteHints(routeHints, amt, hasNode)
		require.Error(t, err)
		require.Contains(t, err.Error(), "no public LSP nodes found")
	})
}
