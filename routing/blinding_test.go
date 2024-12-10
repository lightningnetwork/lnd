package routing

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

// TestBlindedPathValidation tests validation of blinded paths.
func TestBlindedPathValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		payment *BlindedPayment
		err     error
	}{
		{
			name:    "no path",
			payment: &BlindedPayment{},
			err:     ErrNoBlindedPath,
		},
		{
			name: "insufficient hops",
			payment: &BlindedPayment{
				BlindedPath: &sphinx.BlindedPath{
					BlindedHops: []*sphinx.BlindedHopInfo{},
				},
			},
			err: ErrInsufficientBlindedHops,
		},
		{
			name: "maximum < minimum",
			payment: &BlindedPayment{
				BlindedPath: &sphinx.BlindedPath{
					BlindedHops: []*sphinx.BlindedHopInfo{
						{},
					},
				},
				HtlcMaximum: 10,
				HtlcMinimum: 20,
			},
			err: ErrHTLCRestrictions,
		},
		{
			name: "valid",
			payment: &BlindedPayment{
				BlindedPath: &sphinx.BlindedPath{
					BlindedHops: []*sphinx.BlindedHopInfo{
						{},
					},
				},
				HtlcMaximum: 15,
				HtlcMinimum: 5,
			},
		},
	}

	for _, testCase := range tests {
		testCase := testCase

		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			err := testCase.payment.Validate()
			require.ErrorIs(t, err, testCase.err)
		})
	}
}

// TestBlindedPaymentToHints tests conversion of a blinded path to a chain of
// route hints. As our function assumes that the blinded payment has already
// been validated.
func TestBlindedPaymentToHints(t *testing.T) {
	t.Parallel()

	var (
		_, pk1  = btcec.PrivKeyFromBytes([]byte{1})
		_, pkb1 = btcec.PrivKeyFromBytes([]byte{2})
		_, pkb2 = btcec.PrivKeyFromBytes([]byte{3})
		_, pkb3 = btcec.PrivKeyFromBytes([]byte{4})

		v1  = route.NewVertex(pk1)
		vb2 = route.NewVertex(pkb2)
		vb3 = route.NewVertex(pkb3)

		baseFee   uint32 = 1000
		ppmFee    uint32 = 500
		cltvDelta uint16 = 140
		htlcMin   uint64 = 100
		htlcMax   uint64 = 100_000_000

		sizeEncryptedData = 100
		cipherText        = bytes.Repeat(
			[]byte{1}, sizeEncryptedData,
		)
		_, blindedPoint = btcec.PrivKeyFromBytes([]byte{5})

		rawFeatures = lnwire.NewRawFeatureVector(
			lnwire.AMPOptional,
		)

		features = lnwire.NewFeatureVector(
			rawFeatures, lnwire.Features,
		)
	)

	// Create a blinded payment that's just to the introduction node and
	// assert that we get nil hints.
	blindedPayment := &BlindedPayment{
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: pk1,
			BlindingPoint:     blindedPoint,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{},
			},
		},
		BaseFee:             baseFee,
		ProportionalFeeRate: ppmFee,
		CltvExpiryDelta:     cltvDelta,
		HtlcMinimum:         htlcMin,
		HtlcMaximum:         htlcMax,
		Features:            features,
	}
	hints, err := blindedPayment.toRouteHints()
	require.NoError(t, err)
	require.Nil(t, hints)

	// Populate the blinded payment with hops.
	blindedPayment.BlindedPath.BlindedHops = []*sphinx.BlindedHopInfo{
		{
			BlindedNodePub: pkb1,
			CipherText:     cipherText,
		},
		{
			BlindedNodePub: pkb2,
			CipherText:     cipherText,
		},
		{
			BlindedNodePub: pkb3,
			CipherText:     cipherText,
		},
	}

	policy1 := &models.CachedEdgePolicy{
		TimeLockDelta: cltvDelta,
		MinHTLC:       lnwire.MilliSatoshi(htlcMin),
		MaxHTLC:       lnwire.MilliSatoshi(htlcMax),
		FeeBaseMSat:   lnwire.MilliSatoshi(baseFee),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			ppmFee,
		),
		ToNodePubKey: func() route.Vertex {
			return vb2
		},
		ToNodeFeatures: features,
	}
	policy2 := &models.CachedEdgePolicy{
		ToNodePubKey: func() route.Vertex {
			return vb3
		},
		ToNodeFeatures: features,
	}

	blindedEdge1, err := NewBlindedEdge(policy1, blindedPayment, 0)
	require.NoError(t, err)

	blindedEdge2, err := NewBlindedEdge(policy2, blindedPayment, 1)
	require.NoError(t, err)

	expected := RouteHints{
		v1: {
			blindedEdge1,
		},
		vb2: {
			blindedEdge2,
		},
	}

	actual, err := blindedPayment.toRouteHints()
	require.NoError(t, err)

	require.Equal(t, len(expected), len(actual))
	for vertex, expectedHint := range expected {
		actualHint, ok := actual[vertex]
		require.True(t, ok, "node not found: %v", vertex)

		require.Len(t, expectedHint, 1)
		require.Len(t, actualHint, 1)

		// We can't assert that our functions are equal, so we check
		// their output and then mark them as nil so that we can use
		// require.Equal for all our other fields.
		require.Equal(t, expectedHint[0].EdgePolicy().ToNodePubKey(),
			actualHint[0].EdgePolicy().ToNodePubKey())

		actualHint[0].EdgePolicy().ToNodePubKey = nil
		expectedHint[0].EdgePolicy().ToNodePubKey = nil

		// The arguments we use for the payload do not matter as long as
		// both functions return the same payload.
		expectedPayloadSize := expectedHint[0].IntermediatePayloadSize(
			0, 0, 0,
		)
		actualPayloadSize := actualHint[0].IntermediatePayloadSize(
			0, 0, 0,
		)

		require.Equal(t, expectedPayloadSize, actualPayloadSize)

		require.Equal(t, expectedHint[0], actualHint[0])
	}
}

// TestBlindedPaymentDeepCopy tests the deep copy method of the BLindedPayment
// struct.
//
// TODO(ziggie): Make this a property test instead.
func TestBlindedPaymentDeepCopy(t *testing.T) {
	_, pkBlind1 := btcec.PrivKeyFromBytes([]byte{1})
	_, blindingPoint := btcec.PrivKeyFromBytes([]byte{2})
	_, pkBlind2 := btcec.PrivKeyFromBytes([]byte{3})

	// Create a test BlindedPayment with non-nil fields
	original := &BlindedPayment{
		BaseFee:             1000,
		ProportionalFeeRate: 2000,
		CltvExpiryDelta:     144,
		HtlcMinimum:         1000,
		HtlcMaximum:         1000000,
		Features:            lnwire.NewFeatureVector(nil, nil),
		BlindedPath: &sphinx.BlindedPath{
			IntroductionPoint: pkBlind1,
			BlindingPoint:     blindingPoint,
			BlindedHops: []*sphinx.BlindedHopInfo{
				{
					BlindedNodePub: pkBlind2,
					CipherText:     []byte("test cipher"),
				},
			},
		},
	}

	// Make a deep copy
	cpyPayment := original.deepCopy()

	// Test 1: Verify the copy is not the same pointer
	if cpyPayment == original {
		t.Fatal("deepCopy returned same pointer")
	}

	// Verify all fields are equal
	if !reflect.DeepEqual(original, cpyPayment) {
		t.Fatal("copy is not equal to original")
	}

	// Modify the copy and verify it doesn't affect the original
	cpyPayment.BaseFee = 2000
	cpyPayment.BlindedPath.BlindedHops[0].CipherText = []byte("modified")

	require.NotEqual(t, original.BaseFee, cpyPayment.BaseFee)

	require.NotEqual(
		t,
		original.BlindedPath.BlindedHops[0].CipherText,
		cpyPayment.BlindedPath.BlindedHops[0].CipherText,
	)

	// Verify nil handling.
	var nilPayment *BlindedPayment
	nilCopy := nilPayment.deepCopy()
	require.Nil(t, nilCopy)
}
