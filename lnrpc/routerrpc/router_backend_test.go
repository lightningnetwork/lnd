package routerrpc

import (
	"bytes"
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/bolt12"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnmock"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
	paymentsdb "github.com/lightningnetwork/lnd/payments/db"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/routing/trampoline"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// TestMarshallOpaqueTrampolineFailure verifies a relay-local nested failure is
// not exposed as a decoded wire message through the payment RPC.
func TestMarshallOpaqueTrampolineFailure(t *testing.T) {
	t.Parallel()

	failure, err := marshallHtlcFailure(&paymentsdb.HTLCFailInfo{
		Reason: paymentsdb.HTLCFailTrampoline,
	})
	require.NoError(t, err)
	require.Equal(t, lnrpc.Failure_UNREADABLE_FAILURE, failure.Code)
}

// TestTrampolineOnionRPC verifies that the explicit type-20 field round trips
// through route RPC conversion and rejects malformed or intermediate use.
func TestTrampolineOnionRPC(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x31}, 32))
	packet := &sphinx.OnionPacket{
		EphemeralKey: key.PubKey(),
		RoutingInfo:  bytes.Repeat([]byte{0x17}, 97),
	}
	onion, err := trampoline.EncodeOnionPacket(packet)
	require.NoError(t, err)

	hop, err := UnmarshallHopWithPubkey(&lnrpc.Hop{
		TrampolineOnion: onion,
	}, node1)
	require.NoError(t, err)
	require.Equal(t, onion, hop.TrampolineOnion)

	backend := &RouterBackend{
		SelfNode:          sourceKey,
		TrampolineRouting: true,
	}
	rpcRoute := &lnrpc.Route{
		TotalAmtMsat:  1_000,
		TotalTimeLock: 120,
		Hops: []*lnrpc.Hop{{
			PubKey:           hex.EncodeToString(node1[:]),
			AmtToForwardMsat: 1_000,
			Expiry:           110,
			TrampolineOnion:  onion,
		}},
	}
	unmarshalled, err := backend.UnmarshallRoute(rpcRoute)
	require.NoError(t, err)
	require.Equal(t, onion, unmarshalled.Hops[0].TrampolineOnion)
	disabled := &RouterBackend{SelfNode: sourceKey}
	_, err = disabled.UnmarshallRoute(rpcRoute)
	require.ErrorContains(t, err, "trampoline routing is disabled")

	marshalled, err := backend.MarshallRoute(unmarshalled)
	require.NoError(t, err)
	require.Equal(t, onion, marshalled.Hops[0].TrampolineOnion)

	rpcRoute.Hops = append(rpcRoute.Hops, &lnrpc.Hop{
		PubKey:           hex.EncodeToString(node2[:]),
		AmtToForwardMsat: 1_000,
		Expiry:           110,
	})
	_, err = backend.UnmarshallRoute(rpcRoute)
	require.ErrorContains(t, err, "only be provided for the final hop")

	_, err = UnmarshallHopWithPubkey(&lnrpc.Hop{
		TrampolineOnion: []byte{1, 2, 3},
	}, node1)
	require.ErrorContains(t, err, "trampoline onion")
}

// TestMarshallTrampolineFailures verifies that callers can inspect every
// trampoline failure and the fee and expiry quote carried by the budget
// failure.
func TestMarshallTrampolineFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		failure  lnwire.FailureMessage
		expected *lnrpc.Failure
	}{
		{
			name:    "temporary",
			failure: &lnwire.FailTemporaryTrampolineFailure{},
			expected: &lnrpc.Failure{
				Code: lnrpc.
					Failure_TEMPORARY_TRAMPOLINE_FAILURE,
			},
		},
		{
			name: "fee and expiry",
			failure: &lnwire.FailTrampolineFeeOrExpiryInsufficient{
				FeeBaseMsat:               1_000,
				FeeProportionalMillionths: 2_500,
				CltvExpiryDelta:           144,
			},
			expected: &lnrpc.Failure{
				Code: lnrpc.
					Failure_TRAMPOLINE_FEE_OR_EXPIRY_INSUFFICIENT, //nolint:ll
				TrampolineFeeBaseMsat:               1_000,
				TrampolineFeeProportionalMillionths: 2_500,
				TrampolineCltvExpiryDelta:           144,
			},
		},
		{
			name:    "unknown next trampoline",
			failure: &lnwire.FailUnknownNextTrampoline{},
			expected: &lnrpc.Failure{
				Code: lnrpc.Failure_UNKNOWN_NEXT_TRAMPOLINE,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response := &lnrpc.Failure{}
			err := marshallWireError(test.failure, response)
			require.NoError(t, err)
			require.Equal(t, test.expected, response)
		})
	}
}

// TestApplyBolt11Trampoline verifies that the automatic RPC path accumulates
// relay policies backwards, uses a fresh outer secret, and builds the complete
// authenticated inner onion.
func TestApplyBolt11Trampoline(t *testing.T) {
	t.Parallel()

	bobKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x51}, 32))
	carolKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x52}, 32))
	daveKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x53}, 32))
	hash := [32]byte{1, 2, 3}
	invoiceSecret := [32]byte{4, 5, 6}
	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.TrampolineRoutingOptional,
		), lnwire.Features,
	)
	invoice := &zpay32.Invoice{
		PaymentHash: &hash,
		PaymentAddr: fn.Some(invoiceSecret),
		Destination: daveKey.PubKey(),
		Features:    features,
		Metadata:    []byte{7, 8},
	}
	featureByNode := map[route.Vertex]*lnwire.FeatureVector{
		route.NewVertex(bobKey.PubKey()):   features,
		route.NewVertex(carolKey.PubKey()): features,
	}
	backend := &RouterBackend{
		FetchNodeFeatures: func(node route.Vertex) (
			*lnwire.FeatureVector, error) {

			return featureByNode[node], nil
		},
		CurrentHeight: func() uint32 {
			return 500
		},
	}
	request := &SendPaymentRequest{
		TrampolineHops: []*TrampolineHop{
			{
				PubKey: bobKey.PubKey().
					SerializeCompressed(),
				FeeMsat:         100,
				CltvExpiryDelta: 40,
			},
			{
				PubKey: carolKey.PubKey().
					SerializeCompressed(),
				FeeMsat:         200,
				CltvExpiryDelta: 30,
			},
		},
		TrampolinePaddingBytes: 19,
	}
	payment := &routing.LightningPayment{
		Amount:         100_000,
		FinalCLTVDelta: 80,
		PaymentAddr:    fn.Some(invoiceSecret),
		Metadata:       []byte{7, 8},
	}

	err := backend.applyBolt11Trampoline(request, invoice, payment)
	require.NoError(t, err)
	require.Equal(t, route.NewVertex(bobKey.PubKey()), payment.Target)
	require.Equal(t, lnwire.MilliSatoshi(100_300), payment.Amount)
	require.Equal(t, uint16(150), payment.FinalCLTVDelta)
	require.NotEqual(
		t, invoiceSecret, payment.PaymentAddr.UnsafeFromSome(),
	)
	require.Nil(t, payment.Metadata)

	packet, err := trampoline.DecodeOnionPacket(payment.TrampolineOnion)
	require.NoError(t, err)
	bobRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: bobKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err := bobRouter.ProcessOnionPacket(packet, hash[:], 0)
	require.NoError(t, err)
	payload, _, err := trampoline.DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(100_200), *payload.AmountToForward)
	require.Equal(t, uint32(610), *payload.OutgoingCLTV)
	require.Equal(t, carolKey.PubKey(), payload.OutgoingNodeID)

	carolRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: carolKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err = carolRouter.ProcessOnionPacket(
		processed.NextPacket, hash[:], 0,
	)
	require.NoError(t, err)
	payload, _, err = trampoline.DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(100_000), *payload.AmountToForward)
	require.Equal(t, uint32(580), *payload.OutgoingCLTV)
	require.Equal(t, daveKey.PubKey(), payload.OutgoingNodeID)
}

// TestApplyBolt12Trampoline verifies automatic path policy accumulation and
// construction of a clear relay followed by a real blinded trampoline path.
func TestApplyBolt12Trampoline(t *testing.T) {
	t.Parallel()

	relayKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x61}, 32))
	introKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x62}, 32))
	finalKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x63}, 32))
	blindingSession, _ := btcec.PrivKeyFromBytes(
		bytes.Repeat([]byte{0x64}, 32),
	)
	hash := lntypes.Hash{1, 2, 3}
	pathID := bytes.Repeat([]byte{0x65}, 32)

	introData, err := record.EncodeBlindedRouteData(
		record.NewDummyHopRouteData(
			finalKey.PubKey(),
			record.PaymentRelayInfo{
				CltvExpiryDelta: 10,
				BaseFee:         100,
			},
			record.PaymentConstraints{
				MaxCltvExpiry:   600,
				HtlcMinimumMsat: 1,
			},
		),
	)
	require.NoError(t, err)
	finalData, err := record.EncodeBlindedRouteData(
		record.NewFinalHopBlindedRouteData(nil, pathID),
	)
	require.NoError(t, err)
	blinded, err := sphinx.BuildBlindedPath(
		blindingSession, []*sphinx.HopInfo{
			{NodePub: introKey.PubKey(), PlainText: introData},
			{NodePub: finalKey.PubKey(), PlainText: finalData},
		},
	)
	require.NoError(t, err)
	introduction, err := lnwire.NewPubkeyIntro(introKey.PubKey())
	require.NoError(t, err)
	wirePath := lnwire.BlindedPath{
		IntroductionNode: introduction,
		BlindingPoint:    blinded.Path.BlindingPoint,
		Hops: make(
			[]lnwire.BlindedHop, len(blinded.Path.BlindedHops),
		),
	}
	for i := range blinded.Path.BlindedHops {
		hop := blinded.Path.BlindedHops[i]
		wirePath.Hops[i] = lnwire.BlindedHop{
			BlindedNodeID: hop.BlindedNodePub,
			EncryptedData: hop.CipherText,
		}
	}

	features := lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(
			lnwire.TLVOnionPayloadOptional,
			lnwire.PaymentAddrOptional,
			lnwire.MPPOptional,
			lnwire.TrampolineRoutingOptional,
		), lnwire.Features,
	)
	invoice := &bolt12.Invoice{
		InvoicePaths: tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType160, lnwire.BlindedPaths](
				lnwire.BlindedPaths{
					Paths: []lnwire.BlindedPath{
						wirePath, wirePath,
					},
				},
			),
		),
		InvoiceBlindedPay: tlv.SomeRecordT(
			tlv.NewRecordT[
				tlv.TlvType162, bolt12.BlindedPayInfos,
			](bolt12.BlindedPayInfos{
				Infos: []bolt12.BlindedPayInfo{{
					// AMP is known globally but unsupported
					// here. Select the supported path.
					Features: *lnwire.NewRawFeatureVector(
						lnwire.AMPRequired,
					),
					FeeBaseMsat:     9_000,
					CltvExpiryDelta: 20,
					HtlcMaximumMsat: 10_000,
				}, {
					FeeBaseMsat:     100,
					CltvExpiryDelta: 20,
					HtlcMaximumMsat: 10_000,
				}},
			}),
		),
	}
	featureByNode := map[route.Vertex]*lnwire.FeatureVector{
		route.NewVertex(relayKey.PubKey()): features,
		route.NewVertex(introKey.PubKey()): features,
	}
	backend := &RouterBackend{
		FetchNodeFeatures: func(node route.Vertex) (
			*lnwire.FeatureVector, error) {

			return featureByNode[node], nil
		},
		CurrentHeight: func() uint32 {
			return 500
		},
	}
	request := &SendPaymentRequest{
		TrampolineHops: []*TrampolineHop{{
			PubKey: relayKey.PubKey().
				SerializeCompressed(),
			FeeMsat:         50,
			CltvExpiryDelta: 10,
		}},
	}
	payment := &routing.LightningPayment{
		Amount:       1_000,
		DestFeatures: features,
	}
	require.NoError(t, payment.SetPaymentHash(hash))
	oversized, ok := proto.Clone(request).(*SendPaymentRequest)
	require.True(t, ok)
	oversized.TrampolinePaddingBytes = math.MaxUint16 + 1
	err = backend.applyBolt12Trampoline(oversized, invoice, payment)
	require.ErrorContains(t, err, "trampoline padding")
	nilHop, ok := proto.Clone(request).(*SendPaymentRequest)
	require.True(t, ok)
	nilHop.TrampolineHops = []*TrampolineHop{nil}
	err = backend.applyBolt12Trampoline(nilHop, invoice, payment)
	require.ErrorContains(t, err, "trampoline hop 0 is nil")

	err = backend.applyBolt12Trampoline(request, invoice, payment)
	require.NoError(t, err)
	require.Equal(t, route.NewVertex(relayKey.PubKey()), payment.Target)
	require.Equal(t, lnwire.MilliSatoshi(1_150), payment.Amount)
	require.Equal(t, uint16(30), payment.FinalCLTVDelta)
	require.NotEmpty(t, payment.TrampolineOnion)

	packet, err := trampoline.DecodeOnionPacket(payment.TrampolineOnion)
	require.NoError(t, err)
	relayRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: relayKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err := relayRouter.ProcessOnionPacket(packet, hash[:], 0)
	require.NoError(t, err)
	payload, _, err := trampoline.DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(1_100), *payload.AmountToForward)
	require.Equal(t, uint32(520), *payload.OutgoingCLTV)
	require.Equal(t, introKey.PubKey(), payload.OutgoingNodeID)

	introRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: introKey},
		sphinx.NewNoOpReplayLog(),
	)
	processed, err = introRouter.ProcessOnionPacket(
		processed.NextPacket, hash[:], 0,
	)
	require.NoError(t, err)
	payload, _, err = trampoline.DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(
		t, blinded.Path.BlindingPoint, payload.CurrentPathKey,
	)
}

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

// TestUnmarshallHopBlindedFieldsRequireEncryptedData verifies that callers
// cannot submit partial blinded-hop data through SendToRouteV2.
func TestUnmarshallHopBlindedFieldsRequireEncryptedData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		hop     *lnrpc.Hop
		errText string
	}{
		{
			name: "total amount without encrypted data",
			hop: &lnrpc.Hop{
				TotalAmtMsat: 1000,
			},
			errText: "encrypted data should be present",
		},
		{
			name: "total amount with encrypted data",
			hop: &lnrpc.Hop{
				TotalAmtMsat:  1000,
				EncryptedData: []byte{1},
			},
		},
		{
			name: "ordinary hop",
			hop:  &lnrpc.Hop{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := UnmarshallHopWithPubkey(test.hop, node1)
			if test.errText != "" {
				require.ErrorContains(t, err, test.errText)
				return
			}

			require.NoError(t, err)
		})
	}
}

// TestUnmarshallRouteBlindedTotalAmountFinalHop verifies that the blinded
// total amount is only accepted on the final hop of a caller-provided route.
func TestUnmarshallRouteBlindedTotalAmountFinalHop(t *testing.T) {
	t.Parallel()

	blindedHop := func() *lnrpc.Hop {
		return &lnrpc.Hop{
			PubKey:        destKey,
			EncryptedData: []byte{1},
			TotalAmtMsat:  1000,
		}
	}
	regularHop := func() *lnrpc.Hop {
		return &lnrpc.Hop{
			PubKey: destKey,
		}
	}

	tests := []struct {
		name    string
		hops    []*lnrpc.Hop
		errText string
	}{
		{
			name: "intermediate hop",
			hops: []*lnrpc.Hop{
				blindedHop(), regularHop(),
			},
			errText: "blinded total amount can only be provided " +
				"for the final hop",
		},
		{
			name: "final hop",
			hops: []*lnrpc.Hop{
				regularHop(), blindedHop(),
			},
		},
	}

	backend := &RouterBackend{
		SelfNode: sourceKey,
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			_, err := backend.UnmarshallRoute(&lnrpc.Route{
				Hops: test.hops,
			})
			if test.errText != "" {
				require.ErrorContains(t, err, test.errText)
				return
			}

			require.NoError(t, err)
		})
	}
}

// TestQueryRoutes asserts that query routes rpc parameters are properly parsed
// and passed onto path finding.
func TestQueryRoutes(t *testing.T) {
	t.Run("no mission control", func(t *testing.T) {
		testQueryRoutes(t, false, false, true)
	})
	t.Run("no mission control and msat", func(t *testing.T) {
		testQueryRoutes(t, false, true, true)
	})
	t.Run("with mission control", func(t *testing.T) {
		testQueryRoutes(t, true, false, true)
	})
	t.Run("no mission control bad cltv limit", func(t *testing.T) {
		testQueryRoutes(t, false, false, false)
	})
}

func testQueryRoutes(t *testing.T, useMissionControl bool, useMsat bool,
	setTimelock bool) {

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

	request.OutgoingChanIds = outgoingChanIds

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

		require.Equal(
			t, restrictions.OutgoingChannelIDs, outgoingChanIds,
		)

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

	innerKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x35}, 32))
	innerPacket := &sphinx.OnionPacket{
		EphemeralKey: innerKey.PubKey(),
		RoutingInfo:  bytes.Repeat([]byte{0x19}, 97),
	}
	explicitOnion, err := trampoline.EncodeOnionPacket(innerPacket)
	require.NoError(t, err)
	validExplicitRequest := func() *SendPaymentRequest {
		return &SendPaymentRequest{
			Dest:                      bytes.Clone(destNodeBytes),
			Amt:                       int64(paymentAmount),
			PaymentHash:               make([]byte, 32),
			PaymentAddr:               make([]byte, 32),
			TrampolineOnion:           bytes.Clone(explicitOnion),
			TrampolineErrorSessionKey: innerKey.Serialize(),
			TrampolineErrorPath: [][]byte{
				bytes.Clone(destNodeBytes),
			},
			DestFeatures: []lnrpc.FeatureBit{
				lnrpc.FeatureBit_TLV_ONION_OPT,
				lnrpc.FeatureBit_PAYMENT_ADDR_OPT,
				lnrpc.FeatureBit_MPP_OPT,
				lnrpc.FeatureBit_TRAMPOLINE_ROUTING_OPT,
			},
		}
	}
	validExplicitBackend := func() *RouterBackend {
		return &RouterBackend{
			MaxTotalTimelock:  1000,
			TrampolineRouting: true,
			ShouldSetExpAccountability: func() bool {
				return false
			},
		}
	}

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
			name:    "Malformed trampoline onion",
			backend: &RouterBackend{TrampolineRouting: true},
			sendReq: &SendPaymentRequest{
				TrampolineOnion: []byte{1, 2, 3},
			},
			valid:            false,
			expectedErrorMsg: "trampoline onion",
		},
		{
			name:    "Trampoline routing disabled",
			backend: &RouterBackend{},
			sendReq: &SendPaymentRequest{
				TrampolineHops: []*TrampolineHop{{}},
			},
			valid:            false,
			expectedErrorMsg: "trampoline routing is disabled",
		},
		{
			name: "Explicit trampoline onion missing " +
				"error metadata",
			backend: validExplicitBackend(),
			sendReq: func() *SendPaymentRequest {
				req := validExplicitRequest()
				req.TrampolineErrorSessionKey = nil
				req.TrampolineErrorPath = nil

				return req
			}(),
			valid: false,
			expectedErrorMsg: "trampoline_onion requires " +
				"trampoline_error_session_key",
		},
		{
			name: "Explicit trampoline error metadata " +
				"incomplete",
			backend: validExplicitBackend(),
			sendReq: func() *SendPaymentRequest {
				req := validExplicitRequest()
				req.TrampolineErrorPath = nil

				return req
			}(),
			valid:            false,
			expectedErrorMsg: "must be supplied together",
		},
		{
			name:    "Explicit trampoline error key invalid",
			backend: validExplicitBackend(),
			sendReq: func() *SendPaymentRequest {
				req := validExplicitRequest()
				req.TrampolineErrorSessionKey = make([]byte, 32)

				return req
			}(),
			valid: false,
			expectedErrorMsg: "invalid trampoline error " +
				"session key",
		},
		{
			name:    "Explicit trampoline error path invalid",
			backend: validExplicitBackend(),
			sendReq: func() *SendPaymentRequest {
				req := validExplicitRequest()
				req.TrampolineErrorPath = [][]byte{{1}}

				return req
			}(),
			valid:            false,
			expectedErrorMsg: "trampoline error path hop 0",
		},
		{
			name:    "Explicit trampoline error path mismatch",
			backend: validExplicitBackend(),
			sendReq: func() *SendPaymentRequest {
				req := validExplicitRequest()
				req.TrampolineErrorPath = [][]byte{
					innerKey.PubKey().SerializeCompressed(),
				}

				return req
			}(),
			valid: false,
			expectedErrorMsg: "first trampoline error path hop " +
				"must match",
		},
		{
			name:    "Valid explicit trampoline error metadata",
			backend: validExplicitBackend(),
			sendReq: validExplicitRequest(),
			valid:   true,
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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
				ShouldSetExpAccountability: func() bool {
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

// TestMarshallRouteChanCapacity verifies that MarshallRoute correctly sets the
// ChanCapacity for each hop based on the incoming amount at that hop, not
// the total route amount. This is a regression test to ensure the
// incomingAmt is updated per hop.
func TestMarshallRouteChanCapacity(t *testing.T) {
	t.Parallel()

	// Build a two-hop route: source -> hop1 -> hop2 -> dest.
	//
	// TotalAmount (incoming to hop1) = 1000 msat
	// hop1.AmtToForward (incoming to hop2) = 900 msat (after fee)
	const (
		totalAmtMsat = lnwire.MilliSatoshi(1000)
		hop1Forward  = lnwire.MilliSatoshi(900)
		hop2Forward  = lnwire.MilliSatoshi(900)
	)

	hops := []*route.Hop{
		{
			ChannelID:    1,
			AmtToForward: hop1Forward,
			PubKeyBytes:  node1,
		},
		{
			ChannelID:    2,
			AmtToForward: hop2Forward,
			PubKeyBytes:  node2,
		},
	}

	r, err := route.NewRouteFromHops(totalAmtMsat, 100, sourceKey, hops)
	require.NoError(t, err)

	backend := &RouterBackend{}
	rpcRoute, err := backend.MarshallRoute(r)
	require.NoError(t, err)
	require.Len(t, rpcRoute.Hops, 2)

	// The first hop's capacity should reflect the total incoming amount
	// (route.TotalAmount), converted to satoshis.
	require.EqualValues(
		t, totalAmtMsat.ToSatoshis(), rpcRoute.Hops[0].ChanCapacity,
	)

	// The second hop's capacity should reflect hop1's forwarded amount, not
	// the total route amount. Before the fix, both hops incorrectly used
	// the total route amount.
	require.EqualValues(
		t, hop1Forward.ToSatoshis(), rpcRoute.Hops[1].ChanCapacity,
	)
}
