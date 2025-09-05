package routerrpc

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

const (
	destKey       = "0286098b97bc843372b4426d4b276cea9aa2f48f0428d6f5b66ae101befc14f8b4"
	ignoreNodeKey = "02f274f48f3c0d590449a6776e3ce8825076ac376e470e992246eebc565ef8bb2a"
	hintNodeKey   = "0274e7fb33eafd74fe1acb6db7680bb4aa78e9c839a6e954e38abfad680f645ef7"

	testMissionControlProb = 0.5
)

var (
	sourceKey = route.Vertex{1, 2, 3}

	node1 = route.Vertex{10}

	node2 = route.Vertex{11}
)

var (
	singleChanID = "singleChanID"
	multiChanID  = "multiChanID"
	bothChanIds  = "bothChanIds"
)

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
	t.Run("no mission control", func(t *testing.T) {
		testQueryRoutes(t, false, false, true, singleChanID)
	})
	t.Run("no mission control and msat", func(t *testing.T) {
		testQueryRoutes(t, false, true, true, singleChanID)
	})
	t.Run("with mission control", func(t *testing.T) {
		testQueryRoutes(t, true, false, true, singleChanID)
	})
	t.Run("no mission control bad cltv limit", func(t *testing.T) {
		testQueryRoutes(t, false, false, false, singleChanID)
	})

	t.Run("both outgoing chan id and chan ids", func(t *testing.T) {
		testQueryRoutes(t, true, false, true, bothChanIds)
	})

	t.Run("multiple outgoing chan ids", func(t *testing.T) {
		testQueryRoutes(t, false, true, true, multiChanID)
	})
}

func testQueryRoutes(t *testing.T, useMissionControl bool, useMsat bool,
	setTimelock bool, outgoingChanConfig string) {

	ignoreNodeBytes, err := hex.DecodeString(ignoreNodeKey)
	if err != nil {
		t.Fatal(err)
	}

	var ignoreNodeVertex route.Vertex
	copy(ignoreNodeVertex[:], ignoreNodeBytes)

	destNodeBytes, err := hex.DecodeString(destKey)
	if err != nil {
		t.Fatal(err)
	}

	var (
		lastHop         = route.Vertex{64}
		outgoingChan    = uint64(383322)
		outgoingChanIds = []uint64{383322, 383323}
	)

	hintNode, err := route.NewVertexFromStr(hintNodeKey)
	if err != nil {
		t.Fatal(err)
	}

	rpcRouteHints := []*lnrpc.RouteHint{
		{
			HopHints: []*lnrpc.HopHint{
				{
					ChanId: 38484,
					NodeId: hintNodeKey,
				},
			},
		},
	}

	request := &lnrpc.QueryRoutesRequest{
		PubKey:         destKey,
		FinalCltvDelta: 100,
		IgnoredNodes:   [][]byte{ignoreNodeBytes},
		IgnoredEdges: []*lnrpc.EdgeLocator{{
			ChannelId:        555,
			DirectionReverse: true,
		}},
		IgnoredPairs: []*lnrpc.NodePair{{
			From: node1[:],
			To:   node2[:],
		}},
		UseMissionControl: useMissionControl,
		LastHopPubkey:     lastHop[:],
		DestFeatures:      []lnrpc.FeatureBit{lnrpc.FeatureBit_MPP_OPT},
		RouteHints:        rpcRouteHints,
	}

	amtSat := int64(100000)
	if useMsat {
		request.AmtMsat = amtSat * 1000
		request.FeeLimit = &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_FixedMsat{
				FixedMsat: 250000,
			},
		}
	} else {
		request.Amt = amtSat
		request.FeeLimit = &lnrpc.FeeLimit{
			Limit: &lnrpc.FeeLimit_Fixed{
				Fixed: 250,
			},
		}
	}

	switch outgoingChanConfig {
	case singleChanID:
		request.OutgoingChanId = outgoingChan

	case multiChanID:
		request.OutgoingChanIds = outgoingChanIds

	case bothChanIds:
		request.OutgoingChanId = outgoingChan
		request.OutgoingChanIds = outgoingChanIds
	}

	findRoute := func(req *routing.RouteRequest) (*route.Route, float64,
		error) {

		if int64(req.Amount) != amtSat*1000 {
			t.Fatal("unexpected amount")
		}

		if req.Source != sourceKey {
			t.Fatal("unexpected source key")
		}

		target := req.Target
		if !bytes.Equal(target[:], destNodeBytes) {
			t.Fatal("unexpected target key")
		}

		restrictions := req.Restrictions
		if restrictions.FeeLimit != 250*1000 {
			t.Fatal("unexpected fee limit")
		}

		if restrictions.ProbabilitySource(route.Vertex{2},
			route.Vertex{1}, 0, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored edge")
		}

		if restrictions.ProbabilitySource(ignoreNodeVertex,
			route.Vertex{6}, 0, 0,
		) != 0 {
			t.Fatal("expecting 0% probability for ignored node")
		}

		if restrictions.ProbabilitySource(node1, node2, 0, 0) != 0 {
			t.Fatal("expecting 0% probability for ignored pair")
		}

		if *restrictions.LastHop != lastHop {
			t.Fatal("unexpected last hop")
		}

		switch outgoingChanConfig {
		case singleChanID:
			require.Equal(
				t, restrictions.OutgoingChannelIDs,
				[]uint64{outgoingChan},
			)

		case multiChanID:
			require.Equal(
				t, restrictions.OutgoingChannelIDs,
				outgoingChanIds,
			)
		}

		if !restrictions.DestFeatures.HasFeature(lnwire.MPPOptional) {
			t.Fatal("unexpected dest features")
		}

		routeHints := req.RouteHints
		if _, ok := routeHints[hintNode]; !ok {
			t.Fatal("expected route hint")
		}

		expectedProb := 1.0
		if useMissionControl {
			expectedProb = testMissionControlProb
		}
		if restrictions.ProbabilitySource(route.Vertex{4},
			route.Vertex{5}, 0, 0,
		) != expectedProb {
			t.Fatal("expecting 100% probability")
		}

		hops := []*route.Hop{{}}
		route, err := route.NewRouteFromHops(
			req.Amount, 144, req.Source, hops,
		)

		return route, expectedProb, err
	}

	backend := &RouterBackend{
		FindRoute: findRoute,
		SelfNode:  route.Vertex{1, 2, 3},
		FetchChannelCapacity: func(chanID uint64) (
			btcutil.Amount, error) {

			return 1, nil
		},
		FetchAmountPairCapacity: func(nodeFrom, nodeTo route.Vertex,
			amount lnwire.MilliSatoshi) (btcutil.Amount, error) {

			return 1, nil
		},
		MissionControl: &mockMissionControl{},
		FetchChannelEndpoints: func(chanID uint64) (route.Vertex,
			route.Vertex, error) {

			if chanID != 555 {
				t.Fatalf("expected endpoints to be fetched for "+
					"channel 555, but got %v instead",
					chanID)
			}
			return route.Vertex{1}, route.Vertex{2}, nil
		},
	}

	// If this is set, we'll populate MaxTotalTimelock. If this is not set,
	// the test will fail as CltvLimit will be 0.
	if setTimelock {
		backend.MaxTotalTimelock = 1000
	}

	resp, err := backend.QueryRoutes(t.Context(), request)

	// If we're using both OutgoingChanId and OutgoingChanIds, we should get
	// an error.
	if outgoingChanConfig == bothChanIds {
		require.Error(t, err)
		return
	}

	// If no MaxTotalTimelock was set for the QueryRoutes request, make
	// sure an error was returned.
	if !setTimelock {
		require.NotEmpty(t, err)
		return
	}

	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Routes) != 1 {
		t.Fatal("expected a single route response")
	}
}

type mockMissionControl struct {
	MissionControl
}

func (m *mockMissionControl) GetProbability(fromNode, toNode route.Vertex,
	amt lnwire.MilliSatoshi, capacity btcutil.Amount) float64 {

	return testMissionControlProb
}

func (m *mockMissionControl) ResetHistory() error {
	return nil
}

func (m *mockMissionControl) GetHistorySnapshot() *routing.MissionControlSnapshot {
	return nil
}

func (m *mockMissionControl) GetPairHistorySnapshot(fromNode,
	toNode route.Vertex) routing.TimedPairResult {

	return routing.TimedPairResult{}
}

type recordParseOutcome byte

const (
	valid recordParseOutcome = iota
	invalid
	norecord
)

type unmarshalMPPTest struct {
	name    string
	mpp     *lnrpc.MPPRecord
	outcome recordParseOutcome
}

// TestUnmarshalMPP checks both positive and negative cases of UnmarshalMPP to
// assert that an MPP record is only returned when both fields are properly
// specified. It also asserts that zero-values for both inputs is also valid,
// but returns a nil record.
func TestUnmarshalMPP(t *testing.T) {
	tests := []unmarshalMPPTest{
		{
			name:    "nil record",
			mpp:     nil,
			outcome: norecord,
		},
		{
			name: "invalid total or addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  nil,
				TotalAmtMsat: 0,
			},
			outcome: invalid,
		},
		{
			name: "valid total only",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  nil,
				TotalAmtMsat: 8,
			},
			outcome: invalid,
		},
		{
			name: "valid addr only",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  bytes.Repeat([]byte{0x02}, 32),
				TotalAmtMsat: 0,
			},
			outcome: invalid,
		},
		{
			name: "valid total and invalid addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  []byte{0x02},
				TotalAmtMsat: 8,
			},
			outcome: invalid,
		},
		{
			name: "valid total and valid addr",
			mpp: &lnrpc.MPPRecord{
				PaymentAddr:  bytes.Repeat([]byte{0x02}, 32),
				TotalAmtMsat: 8,
			},
			outcome: valid,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testUnmarshalMPP(t, test)
		})
	}
}

func testUnmarshalMPP(t *testing.T, test unmarshalMPPTest) {
	mpp, err := UnmarshalMPP(test.mpp)
	switch test.outcome {
	// Valid arguments should result in no error, a non-nil MPP record, and
	// the fields should be set correctly.
	case valid:
		if err != nil {
			t.Fatalf("unable to parse mpp record: %v", err)
		}
		if mpp == nil {
			t.Fatalf("mpp payload should be non-nil")
		}
		if int64(mpp.TotalMsat()) != test.mpp.TotalAmtMsat {
			t.Fatalf("incorrect total msat")
		}
		addr := mpp.PaymentAddr()
		if !bytes.Equal(addr[:], test.mpp.PaymentAddr) {
			t.Fatalf("incorrect payment addr")
		}

	// Invalid arguments should produce a failure and nil MPP record.
	case invalid:
		if err == nil {
			t.Fatalf("expected failure for invalid mpp")
		}
		if mpp != nil {
			t.Fatalf("mpp payload should be nil for failure")
		}

	// Arguments that produce no MPP field should return no error and no MPP
	// record.
	case norecord:
		if err != nil {
			t.Fatalf("failure for args resulting for no-mpp")
		}
		if mpp != nil {
			t.Fatalf("mpp payload should be nil for no-mpp")
		}

	default:
		t.Fatalf("test case has non-standard outcome")
	}
}

type unmarshalAMPTest struct {
	name    string
	amp     *lnrpc.AMPRecord
	outcome recordParseOutcome
}

// TestUnmarshalAMP asserts the behavior of decoding an RPC AMPRecord.
func TestUnmarshalAMP(t *testing.T) {
	rootShare := bytes.Repeat([]byte{0x01}, 32)
	setID := bytes.Repeat([]byte{0x02}, 32)

	// All child indexes are valid.
	childIndex := uint32(3)

	tests := []unmarshalAMPTest{
		{
			name:    "nil record",
			amp:     nil,
			outcome: norecord,
		},
		{
			name: "invalid root share invalid set id",
			amp: &lnrpc.AMPRecord{
				RootShare:  []byte{0x01},
				SetId:      []byte{0x02},
				ChildIndex: childIndex,
			},
			outcome: invalid,
		},
		{
			name: "valid root share invalid set id",
			amp: &lnrpc.AMPRecord{
				RootShare:  rootShare,
				SetId:      []byte{0x02},
				ChildIndex: childIndex,
			},
			outcome: invalid,
		},
		{
			name: "invalid root share valid set id",
			amp: &lnrpc.AMPRecord{
				RootShare:  []byte{0x01},
				SetId:      setID,
				ChildIndex: childIndex,
			},
			outcome: invalid,
		},
		{
			name: "valid root share valid set id",
			amp: &lnrpc.AMPRecord{
				RootShare:  rootShare,
				SetId:      setID,
				ChildIndex: childIndex,
			},
			outcome: valid,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			testUnmarshalAMP(t, test)
		})
	}
}

func testUnmarshalAMP(t *testing.T, test unmarshalAMPTest) {
	amp, err := UnmarshalAMP(test.amp)
	switch test.outcome {
	case valid:
		require.NoError(t, err)
		require.NotNil(t, amp)

		rootShare := amp.RootShare()
		setID := amp.SetID()
		require.Equal(t, test.amp.RootShare, rootShare[:])
		require.Equal(t, test.amp.SetId, setID[:])
		require.Equal(t, test.amp.ChildIndex, amp.ChildIndex())

	case invalid:
		require.Error(t, err)
		require.Nil(t, amp)

	case norecord:
		require.NoError(t, err)
		require.Nil(t, amp)

	default:
		t.Fatalf("test case has non-standard outcome")
	}
}

// extractIntentTestCase defines a test case for the
// TestExtractIntentFromSendRequest function. It includes the test name, the
// RouterBackend instance, the SendPaymentRequest to be tested, a boolean
// indicating if the test case is valid, and the expected error message if
// applicable.
type extractIntentTestCase struct {
	name             string
	backend          *RouterBackend
	sendReq          *SendPaymentRequest
	valid            bool
	expectedErrorMsg string
}

// TestExtractIntentFromSendRequest verifies that extractIntentFromSendRequest
// correctly translates a SendPaymentRequest from an RPC client into a
// LightningPayment intent.
func TestExtractIntentFromSendRequest(t *testing.T) {
	const paymentAmount = btcutil.Amount(300_000)

	const paymentReq = "lnbcrt500u1pnh0xflpp56w08q26t896vg2e9mtdkrem320tp" +
		"wws9z9sfr7dw86dx97d90u4sdqqcqzzsxqyz5vqsp5z9945kvfy5g9afmakz" +
		"yrur2t4hhn2tr87un8j0r0e6l5m5zm0fus9qxpqysgqk98c6j7qefdpdmzt4" +
		"g6aykds4ydvf2x9lpngqcfux3hv8qlraan9v3s9296r5w5eh959yzadgh5ck" +
		"gjydgyfxdpumxtuk3p3caugmlqpz5necs"

	const paymentReqMissingAddr = "lnbcrt100p1p70xwfzpp5qqqsyqcyq5rqwzqfq" +
		"qqsyqcyq5rqwzqfqqqsyqcyq5rqwzqfqypqdpl2pkx2ctnv5sxxmmwwd5kge" +
		"tjypeh2ursdae8g6twvus8g6rfwvs8qun0dfjkxaqnp4q0n326hr8v9zprg8" +
		"gsvezcch06gfaqqhde2aj730yg0durunfhv669qypqqqz3uu8wnr7883qzxr" +
		"566nuhled49fx6e6q0jn06w6gpgyznwzxwf8xdmye87kpx0y8lqtcgwywsau" +
		"0jkm66evelkw7cggwlegp4anv3cq62wusm"

	destNodeBytes, err := hex.DecodeString(destKey)
	require.NoError(t, err)

	target, err := route.NewVertexFromBytes(destNodeBytes)
	require.NoError(t, err)

	mockClock := &lnmock.MockClock{}
	mockClock.On("Now").Return(time.Date(2025, 3, 1, 13, 0, 0, 0, time.UTC))

	testCases := []extractIntentTestCase{
		{
			name:    "Time preference out of range",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				TimePref: 2,
			},
			valid:            false,
			expectedErrorMsg: "time preference out of range",
		},
		{
			name:    "Outgoing channel exclusivity violation",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				OutgoingChanId:  38484,
				OutgoingChanIds: []uint64{383322},
			},
			valid: false,
			expectedErrorMsg: "outgoing_chan_id and " +
				"outgoing_chan_ids are mutually exclusive",
		},
		{
			name:    "Invalid last hop pubkey length",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				LastHopPubkey: []byte{1},
			},
			valid:            false,
			expectedErrorMsg: "invalid vertex length",
		},
		{
			name: "total time lock exceeds max allowed",
			backend: &RouterBackend{
				MaxTotalTimelock: 1000,
			},
			sendReq: &SendPaymentRequest{
				CltvLimit: 1001,
			},
			valid: false,
			expectedErrorMsg: "total time lock of 1001 exceeds " +
				"max allowed 1000",
		},
		{
			name:    "Max parts exceed allowed limit",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				MaxParts:         1001,
				MaxShardSizeMsat: 300_000,
			},
			valid: false,
			expectedErrorMsg: "requested max_parts (1001) exceeds" +
				" the allowed upper limit",
		},
		{
			name: "Fee limit conflict, both sat and msat " +
				"specified",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				FeeLimitSat:  1000000,
				FeeLimitMsat: 1000000000,
			},
			valid: false,
			expectedErrorMsg: "sat and msat arguments are " +
				"mutually exclusive",
		},
		{
			name:    "Fee limit cannot be negative",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				FeeLimitSat: -1,
			},
			valid:            false,
			expectedErrorMsg: "amount cannot be negative",
		},
		{
			name: "Dest custom records with type below minimum" +
				" range",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				DestCustomRecords: map[uint64][]byte{
					65530: {1, 2},
				},
			},
			valid:            false,
			expectedErrorMsg: "no custom records with types below",
		},
		{
			name:    "MPP params with keysend payments",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				DestCustomRecords: map[uint64][]byte{
					record.KeySendType: {1, 2},
				},
				MaxShardSizeMsat: 300_000,
			},
			valid: false,
			expectedErrorMsg: "MPP not supported with keysend " +
				"payments",
		},
		{
			name: "Custom record entry with TLV type below " +
				"minimum range",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				FirstHopCustomRecords: map[uint64][]byte{
					65530: {1, 2},
				},
			},
			valid:            false,
			expectedErrorMsg: "custom records entry with TLV type",
		},
		{
			name: "Amount conflict, both sat and msat specified",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return true
				},
			},
			sendReq: &SendPaymentRequest{
				Amt:     int64(paymentAmount),
				AmtMsat: int64(paymentAmount) * 1000,
			},
			valid: false,
			expectedErrorMsg: "sat and msat arguments are " +
				"mutually exclusive",
		},
		{
			name: "Both dest and payment_request provided",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Amt:            int64(paymentAmount),
				PaymentRequest: "test",
				Dest:           destNodeBytes,
			},
			valid: false,
			expectedErrorMsg: "dest and payment_request " +
				"cannot appear together",
		},
		{
			name: "Both payment_hash and payment_request provided",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Amt:            int64(paymentAmount),
				PaymentRequest: "test",
				PaymentHash:    make([]byte, 32),
			},
			valid: false,
			expectedErrorMsg: "payment_hash and payment_request " +
				"cannot appear together",
		},
		{
			name: "Both final_cltv_delta and payment_request " +
				"provided",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Amt:            int64(paymentAmount),
				PaymentRequest: "test",
				FinalCltvDelta: 100,
			},
			valid: false,
			expectedErrorMsg: "final_cltv_delta and " +
				"payment_request cannot appear together",
		},
		{
			name: "Invalid payment request length",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
				ActiveNetParams: &chaincfg.RegressionNetParams,
			},
			sendReq: &SendPaymentRequest{
				Amt:            int64(paymentAmount),
				PaymentRequest: "test",
			},
			valid:            false,
			expectedErrorMsg: "invalid bech32 string length",
		},
		{
			name: "Expired invoice payment request",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
				ActiveNetParams: &chaincfg.RegressionNetParams,
				Clock:           mockClock,
			},
			sendReq: &SendPaymentRequest{
				Amt:            int64(paymentAmount),
				PaymentRequest: paymentReq,
			},
			valid:            false,
			expectedErrorMsg: "invoice expired.",
		},
		{
			name: "Invoice missing payment address",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
				ActiveNetParams:  &chaincfg.RegressionNetParams,
				MaxTotalTimelock: 1000,
				Clock:            mockClock,
			},
			sendReq: &SendPaymentRequest{
				PaymentRequest: paymentReqMissingAddr,
			},
			valid: false,
			expectedErrorMsg: "payment request must contain " +
				"either a payment address or blinded paths",
		},
		{
			name: "Invalid dest vertex length",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Amt:  int64(paymentAmount),
				Dest: []byte{1},
			},
			valid:            false,
			expectedErrorMsg: "invalid vertex length",
		},
		{
			name: "Payment request with missing amount",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:           destNodeBytes,
				FinalCltvDelta: 100,
			},
			valid:            false,
			expectedErrorMsg: "amount must be specified",
		},
		{
			name: "Destination lacks AMP support",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:         destNodeBytes,
				Amt:          int64(paymentAmount),
				Amp:          true,
				DestFeatures: []lnrpc.FeatureBit{},
			},
			valid: false,
			expectedErrorMsg: "destination doesn't " +
				"support AMP payments",
		},
		{
			name: "Invalid payment hash length",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:        destNodeBytes,
				Amt:         int64(paymentAmount),
				PaymentHash: make([]byte, 1),
			},
			valid:            false,
			expectedErrorMsg: "invalid hash length",
		},
		{
			name: "Payment amount exceeds maximum possible amount",
			backend: &RouterBackend{
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:             destNodeBytes,
				Amt:              int64(paymentAmount),
				PaymentHash:      make([]byte, 32),
				MaxParts:         10,
				MaxShardSizeMsat: 300_000,
			},
			valid: false,
			expectedErrorMsg: "payment amount 300000000 mSAT " +
				"exceeds maximum possible amount",
		},
		{
			name: "Reject self-payments if not permitted",
			backend: &RouterBackend{
				MaxTotalTimelock: 1000,
				ShouldSetExpEndorsement: func() bool {
					return false
				},
				SelfNode: target,
			},
			sendReq: &SendPaymentRequest{
				Dest:        destNodeBytes,
				Amt:         int64(paymentAmount),
				PaymentHash: make([]byte, 32),
			},
			valid:            false,
			expectedErrorMsg: "self-payments not allowed",
		},
		{
			name: "Required and optional feature bits set",
			backend: &RouterBackend{
				MaxTotalTimelock: 1000,
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:             destNodeBytes,
				Amt:              int64(paymentAmount),
				PaymentHash:      make([]byte, 32),
				MaxParts:         10,
				MaxShardSizeMsat: 30_000_000,
				DestFeatures: []lnrpc.FeatureBit{
					lnrpc.FeatureBit_GOSSIP_QUERIES_OPT,
					lnrpc.FeatureBit_GOSSIP_QUERIES_REQ,
				},
			},
			valid: true,
		},
		{
			name: "Valid send req parameters, payment settled",
			backend: &RouterBackend{
				MaxTotalTimelock: 1000,
				ShouldSetExpEndorsement: func() bool {
					return false
				},
			},
			sendReq: &SendPaymentRequest{
				Dest:             destNodeBytes,
				Amt:              int64(paymentAmount),
				PaymentHash:      make([]byte, 32),
				MaxParts:         10,
				MaxShardSizeMsat: 30_000_000,
			},
			valid: true,
		},
	}

	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := test.backend.
				extractIntentFromSendRequest(test.sendReq)

			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err,
					test.expectedErrorMsg)
			}
		})
	}
}
