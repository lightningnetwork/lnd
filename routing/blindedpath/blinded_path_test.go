package blindedpath

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/record"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

var (
	pubkeyBytes, _ = hex.DecodeString(
		"598ec453728e0ffe0ae2f5e174243cf58f2" +
			"a3f2c83d2457b43036db568b11093",
	)
	pubKeyY = new(btcec.FieldVal)
	_       = pubKeyY.SetByteSlice(pubkeyBytes)
	pubkey  = btcec.NewPublicKey(new(btcec.FieldVal).SetInt(4), pubKeyY)
)

// TestApplyBlindedPathPolicyBuffer tests blinded policy adjustments.
func TestApplyBlindedPathPolicyBuffer(t *testing.T) {
	tests := []struct {
		name          string
		policyIn      *BlindedHopPolicy
		expectedOut   *BlindedHopPolicy
		incMultiplier float64
		decMultiplier float64
		expectedError string
	}{
		{
			name:          "invalid increase multiplier",
			incMultiplier: 0,
			expectedError: "blinded path policy increase " +
				"multiplier must be greater than or equal to 1",
		},
		{
			name:          "decrease multiplier too small",
			incMultiplier: 1,
			decMultiplier: -1,
			expectedError: "blinded path policy decrease " +
				"multiplier must be in the range [0;1]",
		},
		{
			name:          "decrease multiplier too big",
			incMultiplier: 1,
			decMultiplier: 2,
			expectedError: "blinded path policy decrease " +
				"multiplier must be in the range [0;1]",
		},
		{
			name:          "no change",
			incMultiplier: 1,
			decMultiplier: 1,
			policyIn: &BlindedHopPolicy{
				CLTVExpiryDelta: 1,
				MinHTLCMsat:     2,
				MaxHTLCMsat:     3,
				BaseFee:         4,
				FeeRate:         5,
			},
			expectedOut: &BlindedHopPolicy{
				CLTVExpiryDelta: 1,
				MinHTLCMsat:     2,
				MaxHTLCMsat:     3,
				BaseFee:         4,
				FeeRate:         5,
			},
		},
		{
			name:          "buffer up by 100% and down by 50%",
			incMultiplier: 2,
			decMultiplier: 0.5,
			policyIn: &BlindedHopPolicy{
				CLTVExpiryDelta: 10,
				MinHTLCMsat:     20,
				MaxHTLCMsat:     300,
				BaseFee:         40,
				FeeRate:         50,
			},
			expectedOut: &BlindedHopPolicy{
				CLTVExpiryDelta: 20,
				MinHTLCMsat:     40,
				MaxHTLCMsat:     150,
				BaseFee:         80,
				FeeRate:         100,
			},
		},
		{
			name: "new HTLC minimum larger than OG " +
				"maximum",
			incMultiplier: 2,
			decMultiplier: 1,
			policyIn: &BlindedHopPolicy{
				CLTVExpiryDelta: 10,
				MinHTLCMsat:     20,
				MaxHTLCMsat:     30,
				BaseFee:         40,
				FeeRate:         50,
			},
			expectedOut: &BlindedHopPolicy{
				CLTVExpiryDelta: 20,
				MinHTLCMsat:     20,
				MaxHTLCMsat:     30,
				BaseFee:         80,
				FeeRate:         100,
			},
		},
		{
			name: "new HTLC maximum smaller than OG " +
				"minimum",
			incMultiplier: 1,
			decMultiplier: 0.5,
			policyIn: &BlindedHopPolicy{
				CLTVExpiryDelta: 10,
				MinHTLCMsat:     20,
				MaxHTLCMsat:     30,
				BaseFee:         40,
				FeeRate:         50,
			},
			expectedOut: &BlindedHopPolicy{
				CLTVExpiryDelta: 10,
				MinHTLCMsat:     20,
				MaxHTLCMsat:     30,
				BaseFee:         40,
				FeeRate:         50,
			},
		},
		{
			name: "new HTLC minimum and maximums are not " +
				"compatible",
			incMultiplier: 2,
			decMultiplier: 0.5,
			policyIn: &BlindedHopPolicy{
				CLTVExpiryDelta: 10,
				MinHTLCMsat:     30,
				MaxHTLCMsat:     100,
				BaseFee:         40,
				FeeRate:         50,
			},
			expectedOut: &BlindedHopPolicy{
				CLTVExpiryDelta: 20,
				MinHTLCMsat:     30,
				MaxHTLCMsat:     100,
				BaseFee:         80,
				FeeRate:         100,
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			bufferedPolicy, err := AddPolicyBuffer(
				test.policyIn, test.incMultiplier,
				test.decMultiplier,
			)
			if test.expectedError != "" {
				require.ErrorContains(
					t, err, test.expectedError,
				)

				return
			}

			require.Equal(t, test.expectedOut, bufferedPolicy)
		})
	}
}

// TestBlindedPathAccumulatedPolicyCalc tests the logic for calculating the
// accumulated routing policies of a blinded route against an example mentioned
// in the spec document:
// https://github.com/lightning/bolts/blob/master/proposals/route-blinding.md
func TestBlindedPathAccumulatedPolicyCalc(t *testing.T) {
	t.Parallel()

	// In the spec example, the blinded route is:
	// 	Carol -> Bob -> Alice
	// And Alice chooses the following buffered policy for both the C->B
	// and B->A edges.
	nodePolicy := &record.PaymentRelayInfo{
		FeeRate:         500,
		BaseFee:         100,
		CltvExpiryDelta: 144,
	}

	hopPolicies := []*record.PaymentRelayInfo{
		nodePolicy,
		nodePolicy,
	}

	// Alice's minimum final expiry delta is chosen to be 12.
	aliceMinFinalExpDelta := uint16(12)

	totalBase, totalRate, totalCLTVDelta := calcBlindedPathPolicies(
		hopPolicies, aliceMinFinalExpDelta,
	)

	require.Equal(t, lnwire.MilliSatoshi(201), totalBase)
	require.EqualValues(t, 1001, totalRate)
	require.EqualValues(t, 300, totalCLTVDelta)
}

// TestPadBlindedHopInfo asserts that the padding of blinded hop data is done
// correctly and that it takes the expected number of iterations.
func TestPadBlindedHopInfo(t *testing.T) {
	tests := []struct {
		name               string
		expectedIterations int
		expectedFinalSize  int

		// We will use the PathID field of BlindedRouteData to set an
		// initial payload size. The ints in this list represent the
		// size of each PathID.
		pathIDs []int

		// existingPadding is a map from entry index (based on the
		// pathIDs set) to the number of pre-existing padding bytes to
		// add.
		existingPadding map[int]int

		// prePad is true if all the hop payloads should be pre-padded
		// with a zero length TLV Padding field.
		prePad bool

		// minPayloadSize can be used to set the minimum number of bytes
		// that the resulting records should be.
		minPayloadSize int
	}{
		{
			// If there is only one entry, then no padding is
			// expected.
			name:               "single entry",
			expectedIterations: 1,
			pathIDs:            []int{10},

			// The final size will be 12 since the path ID is 10
			// bytes, and it will be prefixed by type and value
			// bytes.
			expectedFinalSize: 12,
		},
		{
			// Same as the above example but with a minimum final
			// size specified.
			name:               "single entry with min size",
			expectedIterations: 2,
			pathIDs:            []int{10},
			minPayloadSize:     500,
			expectedFinalSize:  504,
		},
		{
			// All the payloads are the same size from the get go
			// meaning that no padding is expected.
			name:               "all start equal",
			expectedIterations: 1,
			pathIDs:            []int{10, 10, 10},

			// The final size will be 12 since the path ID is 10
			// bytes, and it will be prefixed by type and value
			// bytes.
			expectedFinalSize: 12,
		},
		{
			// If the blobs differ by 1 byte it will take 4
			// iterations:
			// 1) padding of 1 is added to entry 2 which will
			//    increase its size by 3 bytes since padding does
			//    not yet exist for it.
			// 2) Now entry 1 will be short 2 bytes. It will be
			//    padded by 2 bytes but again since it is a new
			//    padding field, 4 bytes are added.
			// 3) Finally, entry 2 is padded by 1 extra. Since it
			//    already does have a padding field, this does end
			//    up adding only 1 extra byte.
			// 4) The fourth iteration determines that all are now
			//    the same size.
			name:               "differ by 1 - no pre-padding",
			expectedIterations: 4,
			pathIDs:            []int{4, 3},
			expectedFinalSize:  10,
		},
		{
			// By pre-padding the payloads with a zero byte padding,
			// we can reduce the number of iterations quite a bit.
			name:               "differ by 1 - with pre-padding",
			expectedIterations: 2,
			pathIDs:            []int{4, 3},
			expectedFinalSize:  8,
			prePad:             true,
		},
		{
			name:               "existing padding and diff of 1",
			expectedIterations: 2,
			pathIDs:            []int{10, 11},

			// By adding some existing padding, the type and length
			// field for the padding are already accounted for in
			// the first iteration, and so we only expect two
			// iterations to get the payloads to match size here:
			// one for adding a single extra byte to the smaller
			// payload and another for confirming the sizes match.
			existingPadding:   map[int]int{0: 1, 1: 1},
			expectedFinalSize: 16,
		},
		{
			// In this test, we test a BigSize bucket shift. We do
			// this by setting the initial path ID's of both entries
			// to a 0 size which means the total encoding of those
			// will be 2 bytes (to encode the type and length). Then
			// for the initial padding, we let the first entry be
			// 253 bytes long which is just long enough to be in
			// the second BigSize bucket which uses 3 bytes to
			// encode the value length. We make the second entry
			// 252 bytes which still puts it in the first bucket
			// which uses 1 byte for the length. The difference in
			// overall packet size will be 3 bytes (the first entry
			// has 2 more length bytes and 1 more value byte). So
			// the function will try to pad the second entry by 3
			// bytes (iteration 1). This will however result in the
			// second entry shifting to the second BigSize bucket
			// meaning it will gain an additional 2 bytes for the
			// new length encoding meaning that overall it gains 5
			// bytes in size. This will result in another iteration
			// which will result in padding the first entry with an
			// extra 2 bytes to meet the second entry's new size
			// (iteration 2). One more iteration (3) is then done
			// to confirm that all entries are now the same size.
			name:               "big size bucket shift",
			expectedIterations: 3,

			// We make the path IDs large enough so that
			pathIDs:           []int{0, 0},
			existingPadding:   map[int]int{0: 253, 1: 252},
			expectedFinalSize: 261,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			// If the test includes existing padding, then make sure
			// that the number of existing padding entries is equal
			// to the number of PathID entries.
			if test.existingPadding != nil {
				require.Len(t, test.existingPadding,
					len(test.pathIDs))
			}

			hopDataSet := make([]*hopData, len(test.pathIDs))
			for i, l := range test.pathIDs {
				pathID := tlv.SomeRecordT(
					tlv.NewPrimitiveRecord[tlv.TlvType6](
						make([]byte, l),
					),
				)
				data := &record.BlindedRouteData{
					PathID: pathID,
				}

				if test.existingPadding != nil {
					//nolint:ll
					padding := tlv.SomeRecordT(
						tlv.NewPrimitiveRecord[tlv.TlvType1](
							make([]byte, test.existingPadding[i]),
						),
					)

					data.Padding = padding
				}

				hopDataSet[i] = &hopData{data: data}
			}

			hopInfo, stats, err := padHopInfo(
				hopDataSet, test.prePad, test.minPayloadSize,
			)
			require.NoError(t, err)
			require.Equal(t, test.expectedIterations,
				stats.numIterations)
			require.Equal(t, test.expectedFinalSize,
				stats.finalPaddedSize)

			// We expect all resulting blobs to be the same size.
			for _, info := range hopInfo {
				require.Len(
					t, info.PlainText,
					test.expectedFinalSize,
				)
			}
		})
	}
}

// TestPadBlindedHopInfoBlackBox tests the padHopInfo function via the
// quick.Check testing function. It generates a random set of hopData and
// asserts that the resulting padded set always has the same encoded length.
func TestPadBlindedHopInfoBlackBox(t *testing.T) {
	fn := func(data hopDataList) bool {
		resultList, _, err := padHopInfo(data, true, 0)
		require.NoError(t, err)

		// There should be a resulting sphinx.HopInfo struct for each
		// hopData passed to the padHopInfo function.
		if len(resultList) != len(data) {
			return false
		}

		// There is nothing left to check if input set was empty to
		// start with.
		if len(data) == 0 {
			return true
		}

		// Now, assert that the encoded size of each item is the same.
		// Get the size of the first item as a base point.
		payloadSize := len(resultList[0].PlainText)

		// All the other entries should have the same encoded size.
		for i := 1; i < len(resultList); i++ {
			if len(resultList[i].PlainText) != payloadSize {
				return false
			}
		}

		return true
	}

	require.NoError(t, quick.Check(fn, nil))
}

type hopDataList []*hopData

// Generate returns a random instance of the hopDataList type.
//
// NOTE: this is part of the quick.Generate interface.
func (h hopDataList) Generate(rand *rand.Rand, size int) reflect.Value {
	data := make(hopDataList, rand.Intn(size))
	for i := 0; i < len(data); i++ {
		data[i] = &hopData{
			data:   genBlindedRouteData(rand),
			nodeID: pubkey,
		}
	}

	return reflect.ValueOf(data)
}

// A compile-time check to ensure that hopDataList implements the
// quick.Generator interface.
var _ quick.Generator = (*hopDataList)(nil)

// sometimesDo calls the given function with a 50% probability.
func sometimesDo(fn func(), rand *rand.Rand) {
	if rand.Intn(1) == 0 {
		return
	}

	fn()
}

// genBlindedRouteData generates a random record.BlindedRouteData object.
func genBlindedRouteData(rand *rand.Rand) *record.BlindedRouteData {
	var data record.BlindedRouteData

	sometimesDo(func() {
		data.Padding = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType1](
				make([]byte, rand.Intn(1000000)),
			),
		)
	}, rand)

	sometimesDo(func() {
		data.ShortChannelID = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType2](lnwire.ShortChannelID{
				BlockHeight: rand.Uint32(),
				TxIndex:     rand.Uint32(),
				TxPosition:  uint16(rand.Uint32()),
			}),
		)
	}, rand)

	sometimesDo(func() {
		data.NextNodeID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType4](pubkey),
		)
	}, rand)

	sometimesDo(func() {
		data.PathID = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6](
				make([]byte, rand.Intn(100)),
			),
		)
	}, rand)

	sometimesDo(func() {
		data.NextBlindingOverride = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType8](pubkey),
		)
	}, rand)

	sometimesDo(func() {
		data.RelayInfo = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType10](record.PaymentRelayInfo{
				CltvExpiryDelta: uint16(rand.Uint32()),
				FeeRate:         rand.Uint32(),
				BaseFee: lnwire.MilliSatoshi(
					rand.Uint32(),
				),
			}),
		)
	}, rand)

	sometimesDo(func() {
		data.Constraints = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType12](record.PaymentConstraints{
				MaxCltvExpiry: rand.Uint32(),
				HtlcMinimumMsat: lnwire.MilliSatoshi(
					rand.Uint32(),
				),
			}),
		)
	}, rand)

	return &data
}

// TestBuildBlindedPath tests the logic for constructing a blinded path against
// an example mentioned in this spec document:
// https://github.com/lightning/bolts/blob/master/proposals/route-blinding.md
// This example does not use any dummy hops.
func TestBuildBlindedPath(t *testing.T) {
	// Alice chooses the following path to herself for blinded path
	// construction:
	//    	Carol -> Bob -> Alice.
	// Let's construct the corresponding route.Route for this which will be
	// returned from the `FindRoutes` config callback.
	var (
		privC, pkC = btcec.PrivKeyFromBytes([]byte{1})
		privB, pkB = btcec.PrivKeyFromBytes([]byte{2})
		privA, pkA = btcec.PrivKeyFromBytes([]byte{3})

		carol = route.NewVertex(pkC)
		bob   = route.NewVertex(pkB)
		alice = route.NewVertex(pkA)

		chanCB = uint64(1)
		chanBA = uint64(2)
	)

	realRoute := &route.Route{
		SourcePubKey: carol,
		Hops: []*route.Hop{
			{
				PubKeyBytes: bob,
				ChannelID:   chanCB,
			},
			{
				PubKeyBytes: alice,
				ChannelID:   chanBA,
			},
		},
	}

	realPolicies := map[uint64]*models.ChannelEdgePolicy{
		chanCB: {
			ChannelID: chanCB,
			ToNode:    bob,
		},
		chanBA: {
			ChannelID: chanBA,
			ToNode:    alice,
		},
	}

	paths, err := BuildBlindedPaymentPaths(&BuildBlindedPathCfg{
		FindRoutes: func(_ lnwire.MilliSatoshi) ([]*route.Route,
			error) {

			return []*route.Route{realRoute}, nil
		},
		FetchChannelEdgesByID: func(chanID uint64) (
			*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy, error) {

			return nil, realPolicies[chanID], nil, nil
		},
		BestHeight: func() (uint32, error) {
			return 1000, nil
		},
		// In the spec example, all the policies get replaced with
		// the same static values.
		AddPolicyBuffer: func(_ *BlindedHopPolicy) (
			*BlindedHopPolicy, error) {

			return &BlindedHopPolicy{
				FeeRate:         500,
				BaseFee:         100,
				CLTVExpiryDelta: 144,
				MinHTLCMsat:     1000,
				MaxHTLCMsat:     lnwire.MaxMilliSatoshi,
			}, nil
		},
		PathID:                  []byte{1, 2, 3},
		ValueMsat:               1000,
		MinFinalCLTVExpiryDelta: 12,
		BlocksUntilExpiry:       200,
	})
	require.NoError(t, err)
	require.Len(t, paths, 1)

	path := paths[0]

	// Check that all the accumulated policy values are correct.
	require.EqualValues(t, 201, path.FeeBaseMsat)
	require.EqualValues(t, 1001, path.FeeRate)
	require.EqualValues(t, 300, path.CltvExpiryDelta)
	require.EqualValues(t, 1000, path.HTLCMinMsat)
	require.EqualValues(t, lnwire.MaxMilliSatoshi, path.HTLCMaxMsat)

	// Now we check the hops.
	require.Len(t, path.Hops, 3)

	// Assert that all the encrypted recipient blobs have been padded such
	// that they are all the same size.
	require.Len(t, path.Hops[0].CipherText, len(path.Hops[1].CipherText))
	require.Len(t, path.Hops[1].CipherText, len(path.Hops[2].CipherText))

	// The first hop, should have the real pub key of the introduction
	// node: Carol.
	hop := path.Hops[0]
	require.True(t, hop.BlindedNodePub.IsEqual(pkC))

	// As Carol, let's decode the hop data and assert that all expected
	// values have been included.
	var (
		blindingPoint = path.FirstEphemeralBlindingPoint
		data          *record.BlindedRouteData
	)

	// Check that Carol's info is correct.
	data, blindingPoint = decryptAndDecodeHopData(
		t, privC, blindingPoint, hop.CipherText,
	)

	require.Equal(
		t, lnwire.NewShortChanIDFromInt(chanCB),
		data.ShortChannelID.UnwrapOrFail(t).Val,
	)

	require.Equal(t, record.PaymentRelayInfo{
		CltvExpiryDelta: 144,
		FeeRate:         500,
		BaseFee:         100,
	}, data.RelayInfo.UnwrapOrFail(t).Val)

	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1500,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)

	// Check that all Bob's info is correct.
	hop = path.Hops[1]
	data, blindingPoint = decryptAndDecodeHopData(
		t, privB, blindingPoint, hop.CipherText,
	)

	require.Equal(
		t, lnwire.NewShortChanIDFromInt(chanBA),
		data.ShortChannelID.UnwrapOrFail(t).Val,
	)

	require.Equal(t, record.PaymentRelayInfo{
		CltvExpiryDelta: 144,
		FeeRate:         500,
		BaseFee:         100,
	}, data.RelayInfo.UnwrapOrFail(t).Val)

	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1356,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)

	// Check that all Alice's info is correct.
	hop = path.Hops[2]
	data, _ = decryptAndDecodeHopData(
		t, privA, blindingPoint, hop.CipherText,
	)
	require.True(t, data.ShortChannelID.IsNone())
	require.True(t, data.RelayInfo.IsNone())
	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1212,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)
	require.Equal(t, []byte{1, 2, 3}, data.PathID.UnwrapOrFail(t).Val)
}

// TestBuildBlindedPathWithDummyHops tests the construction of a blinded path
// which includes dummy hops.
func TestBuildBlindedPathWithDummyHops(t *testing.T) {
	// Alice chooses the following path to herself for blinded path
	// construction:
	//    	Carol -> Bob -> Alice.
	// Let's construct the corresponding route.Route for this which will be
	// returned from the `FindRoutes` config callback.
	var (
		privC, pkC = btcec.PrivKeyFromBytes([]byte{1})
		privB, pkB = btcec.PrivKeyFromBytes([]byte{2})
		privA, pkA = btcec.PrivKeyFromBytes([]byte{3})

		carol = route.NewVertex(pkC)
		bob   = route.NewVertex(pkB)
		alice = route.NewVertex(pkA)

		chanCB = uint64(1)
		chanBA = uint64(2)
	)

	realRoute := &route.Route{
		SourcePubKey: carol,
		Hops: []*route.Hop{
			{
				PubKeyBytes: bob,
				ChannelID:   chanCB,
			},
			{
				PubKeyBytes: alice,
				ChannelID:   chanBA,
			},
		},
	}

	realPolicies := map[uint64]*models.ChannelEdgePolicy{
		chanCB: {
			ChannelID: chanCB,
			ToNode:    bob,
		},
		chanBA: {
			ChannelID: chanBA,
			ToNode:    alice,
		},
	}

	paths, err := BuildBlindedPaymentPaths(&BuildBlindedPathCfg{
		FindRoutes: func(_ lnwire.MilliSatoshi) ([]*route.Route,
			error) {

			return []*route.Route{realRoute}, nil
		},
		FetchChannelEdgesByID: func(chanID uint64) (
			*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy, error) {

			policy, ok := realPolicies[chanID]
			if !ok {
				return nil, nil, nil,
					fmt.Errorf("edge not found")
			}

			return nil, policy, nil, nil
		},
		BestHeight: func() (uint32, error) {
			return 1000, nil
		},
		// In the spec example, all the policies get replaced with
		// the same static values.
		AddPolicyBuffer: func(_ *BlindedHopPolicy) (
			*BlindedHopPolicy, error) {

			return &BlindedHopPolicy{
				FeeRate:         500,
				BaseFee:         100,
				CLTVExpiryDelta: 144,
				MinHTLCMsat:     1000,
				MaxHTLCMsat:     lnwire.MaxMilliSatoshi,
			}, nil
		},
		PathID:                  []byte{1, 2, 3},
		ValueMsat:               1000,
		MinFinalCLTVExpiryDelta: 12,
		BlocksUntilExpiry:       200,

		// By setting the minimum number of hops to 4, we force 2 dummy
		// hops to be added to the real route.
		MinNumHops: 4,

		DefaultDummyHopPolicy: &BlindedHopPolicy{
			CLTVExpiryDelta: 50,
			FeeRate:         100,
			BaseFee:         100,
			MinHTLCMsat:     1000,
			MaxHTLCMsat:     lnwire.MaxMilliSatoshi,
		},
	})
	require.NoError(t, err)
	require.Len(t, paths, 1)

	path := paths[0]

	// Check that all the accumulated policy values are correct.
	require.EqualValues(t, 403, path.FeeBaseMsat)
	require.EqualValues(t, 2003, path.FeeRate)
	require.EqualValues(t, 588, path.CltvExpiryDelta)
	require.EqualValues(t, 1000, path.HTLCMinMsat)
	require.EqualValues(t, lnwire.MaxMilliSatoshi, path.HTLCMaxMsat)

	// Now we check the hops.
	require.Len(t, path.Hops, 5)

	// Assert that all the encrypted recipient blobs have been padded such
	// that they are all the same size.
	require.Len(t, path.Hops[0].CipherText, len(path.Hops[1].CipherText))
	require.Len(t, path.Hops[1].CipherText, len(path.Hops[2].CipherText))
	require.Len(t, path.Hops[2].CipherText, len(path.Hops[3].CipherText))
	require.Len(t, path.Hops[3].CipherText, len(path.Hops[4].CipherText))

	// The first hop, should have the real pub key of the introduction
	// node: Carol.
	hop := path.Hops[0]
	require.True(t, hop.BlindedNodePub.IsEqual(pkC))

	// As Carol, let's decode the hop data and assert that all expected
	// values have been included.
	var (
		blindingPoint = path.FirstEphemeralBlindingPoint
		data          *record.BlindedRouteData
	)

	// Check that Carol's info is correct.
	data, blindingPoint = decryptAndDecodeHopData(
		t, privC, blindingPoint, hop.CipherText,
	)

	require.Equal(
		t, lnwire.NewShortChanIDFromInt(chanCB),
		data.ShortChannelID.UnwrapOrFail(t).Val,
	)

	require.Equal(t, record.PaymentRelayInfo{
		CltvExpiryDelta: 144,
		FeeRate:         500,
		BaseFee:         100,
	}, data.RelayInfo.UnwrapOrFail(t).Val)

	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1788,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)

	// Check that all Bob's info is correct.
	hop = path.Hops[1]
	data, blindingPoint = decryptAndDecodeHopData(
		t, privB, blindingPoint, hop.CipherText,
	)

	require.Equal(
		t, lnwire.NewShortChanIDFromInt(chanBA),
		data.ShortChannelID.UnwrapOrFail(t).Val,
	)

	require.Equal(t, record.PaymentRelayInfo{
		CltvExpiryDelta: 144,
		FeeRate:         500,
		BaseFee:         100,
	}, data.RelayInfo.UnwrapOrFail(t).Val)

	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1644,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)

	// Check that all Alice's info is correct. The payload should contain
	// a next_node_id field that is equal to Alice's public key. This
	// indicates to Alice that she should continue peeling the onion.
	hop = path.Hops[2]
	data, blindingPoint = decryptAndDecodeHopData(
		t, privA, blindingPoint, hop.CipherText,
	)
	require.True(t, data.ShortChannelID.IsNone())
	require.True(t, data.RelayInfo.IsSome())
	require.True(t, data.Constraints.IsSome())
	require.Equal(t, pkA, data.NextNodeID.UnwrapOrFail(t).Val)

	// Alice should be able to decrypt the next payload with her private
	// key. This next payload is yet another dummy hop.
	hop = path.Hops[3]
	data, blindingPoint = decryptAndDecodeHopData(
		t, privA, blindingPoint, hop.CipherText,
	)
	require.True(t, data.ShortChannelID.IsNone())
	require.True(t, data.RelayInfo.IsSome())
	require.True(t, data.Constraints.IsSome())
	require.Equal(t, pkA, data.NextNodeID.UnwrapOrFail(t).Val)

	// Unwrapping one more time should reveal the final hop info for Alice.
	hop = path.Hops[4]
	data, _ = decryptAndDecodeHopData(
		t, privA, blindingPoint, hop.CipherText,
	)
	require.True(t, data.ShortChannelID.IsNone())
	require.True(t, data.RelayInfo.IsNone())
	require.Equal(t, record.PaymentConstraints{
		MaxCltvExpiry:   1212,
		HtlcMinimumMsat: 1000,
	}, data.Constraints.UnwrapOrFail(t).Val)
	require.Equal(t, []byte{1, 2, 3}, data.PathID.UnwrapOrFail(t).Val)

	// Demonstrate that BuildBlindedPaymentPaths continues to use any
	// functioning paths even if some routes cant be used to build a blinded
	// path. We do this by forcing FetchChannelEdgesByID to error out for
	// the first 2 calls. FindRoutes returns 3 routes and so by the end, we
	// still get 1 valid path.
	var errCount int
	paths, err = BuildBlindedPaymentPaths(&BuildBlindedPathCfg{
		FindRoutes: func(_ lnwire.MilliSatoshi) ([]*route.Route,
			error) {

			return []*route.Route{realRoute, realRoute, realRoute},
				nil
		},
		FetchChannelEdgesByID: func(chanID uint64) (
			*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
			*models.ChannelEdgePolicy, error) {

			// Force the call to error for the first 2 channels.
			if errCount < 2 {
				errCount++

				return nil, nil, nil,
					fmt.Errorf("edge not found")
			}

			policy, ok := realPolicies[chanID]
			if !ok {
				return nil, nil, nil,
					fmt.Errorf("edge not found")
			}

			return nil, policy, nil, nil
		},
		BestHeight: func() (uint32, error) {
			return 1000, nil
		},
		// In the spec example, all the policies get replaced with
		// the same static values.
		AddPolicyBuffer: func(_ *BlindedHopPolicy) (
			*BlindedHopPolicy, error) {

			return &BlindedHopPolicy{
				FeeRate:         500,
				BaseFee:         100,
				CLTVExpiryDelta: 144,
				MinHTLCMsat:     1000,
				MaxHTLCMsat:     lnwire.MaxMilliSatoshi,
			}, nil
		},
		PathID:                  []byte{1, 2, 3},
		ValueMsat:               1000,
		MinFinalCLTVExpiryDelta: 12,
		BlocksUntilExpiry:       200,

		// By setting the minimum number of hops to 4, we force 2 dummy
		// hops to be added to the real route.
		MinNumHops: 4,

		DefaultDummyHopPolicy: &BlindedHopPolicy{
			CLTVExpiryDelta: 50,
			FeeRate:         100,
			BaseFee:         100,
			MinHTLCMsat:     1000,
			MaxHTLCMsat:     lnwire.MaxMilliSatoshi,
		},
	})
	require.NoError(t, err)
	require.Len(t, paths, 1)
}

// TestSingleHopBlindedPath tests that blinded path construction is done
// correctly for the case where the destination node is also the introduction
// node.
func TestSingleHopBlindedPath(t *testing.T) {
	var (
		_, pkC = btcec.PrivKeyFromBytes([]byte{1})
		carol  = route.NewVertex(pkC)
	)

	realRoute := &route.Route{
		SourcePubKey: carol,
		// No hops since Carol is both the introduction node and the
		// final destination node.
		Hops: []*route.Hop{},
	}

	paths, err := BuildBlindedPaymentPaths(&BuildBlindedPathCfg{
		FindRoutes: func(_ lnwire.MilliSatoshi) ([]*route.Route,
			error) {

			return []*route.Route{realRoute}, nil
		},
		BestHeight: func() (uint32, error) {
			return 1000, nil
		},
		PathID:                  []byte{1, 2, 3},
		ValueMsat:               1000,
		MinFinalCLTVExpiryDelta: 12,
		BlocksUntilExpiry:       200,
	})
	require.NoError(t, err)
	require.Len(t, paths, 1)

	path := paths[0]

	// Check that all the accumulated policy values are correct. Since this
	// is a unique case where the destination node is also the introduction
	// node, the accumulated fee and HTLC values should be zero and the
	// CLTV expiry delta should be equal to Carol's MinFinalCLTVExpiryDelta.
	require.EqualValues(t, 0, path.FeeBaseMsat)
	require.EqualValues(t, 0, path.FeeRate)
	require.EqualValues(t, 0, path.HTLCMinMsat)
	require.EqualValues(t, 0, path.HTLCMaxMsat)
	require.EqualValues(t, 12, path.CltvExpiryDelta)
}

func decryptAndDecodeHopData(t *testing.T, priv *btcec.PrivateKey,
	ephem *btcec.PublicKey, cipherText []byte) (*record.BlindedRouteData,
	*btcec.PublicKey) {

	router := sphinx.NewRouter(
		&keychain.PrivKeyECDH{PrivKey: priv}, nil,
	)

	decrypted, err := router.DecryptBlindedHopData(ephem, cipherText)
	require.NoError(t, err)

	buf := bytes.NewBuffer(decrypted)
	routeData, err := record.DecodeBlindedRouteData(buf)
	require.NoError(t, err)

	nextEphem, err := router.NextEphemeral(ephem)
	require.NoError(t, err)

	return routeData, nextEphem
}
