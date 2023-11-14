package channeldb

import (
	"bytes"
	"encoding/hex"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	testSchnorrSigStr, _ = hex.DecodeString("04E7F9037658A92AFEB4F2" +
		"5BAE5339E3DDCA81A353493827D26F16D92308E49E2A25E9220867" +
		"8A2DF86970DA91B03A8AF8815A8A60498B358DAF560B347AA557")
	testSchnorrSig, _ = lnwire.NewSigFromSchnorrRawSignature(
		testSchnorrSigStr,
	)
)

// TestEdgeInfoSerialisation tests the serialisation and deserialization logic
// for models.ChannelEdgeInfo.
func TestEdgeInfoSerialisation(t *testing.T) {
	t.Parallel()

	mainScenario := func(info models.ChannelEdgeInfo) bool {
		var b bytes.Buffer
		err := serializeChanEdgeInfo(&b, info)
		require.NoError(t, err)

		newInfo, err := deserializeChanEdgeInfo(&b)
		require.NoError(t, err)

		return assert.Equal(t, info, newInfo)
	}

	tests := []struct {
		name     string
		genValue func([]reflect.Value, *rand.Rand)
		scenario any
	}{
		{
			name: "ChannelEdgeInfo1",
			scenario: func(m models.ChannelEdgeInfo1) bool {
				return mainScenario(&m)
			},
			genValue: func(v []reflect.Value, r *rand.Rand) {
				info := &models.ChannelEdgeInfo1{
					ChannelID:        r.Uint64(),
					NodeKey1Bytes:    randRawKey(t),
					NodeKey2Bytes:    randRawKey(t),
					BitcoinKey1Bytes: randRawKey(t),
					BitcoinKey2Bytes: randRawKey(t),
					ChannelPoint: wire.OutPoint{
						Index: r.Uint32(),
					},
					Capacity: btcutil.Amount(
						r.Uint32(),
					),
					ExtraOpaqueData: make([]byte, 0),
				}

				_, err := r.Read(info.ChainHash[:])
				require.NoError(t, err)

				_, err = r.Read(info.ChannelPoint.Hash[:])
				require.NoError(t, err)

				info.Features = make([]byte, r.Intn(900))
				_, err = r.Read(info.Features)
				require.NoError(t, err)

				// Sometimes add an AuthProof.
				if r.Intn(2)%2 == 0 {
					n := r.Intn(80)

					//nolint:lll
					authProof := &models.ChannelAuthProof1{
						NodeSig1Bytes:    make([]byte, n),
						NodeSig2Bytes:    make([]byte, n),
						BitcoinSig1Bytes: make([]byte, n),
						BitcoinSig2Bytes: make([]byte, n),
					}

					_, err = r.Read(
						authProof.NodeSig1Bytes,
					)
					require.NoError(t, err)

					_, err = r.Read(
						authProof.NodeSig2Bytes,
					)
					require.NoError(t, err)

					_, err = r.Read(
						authProof.BitcoinSig1Bytes,
					)
					require.NoError(t, err)

					_, err = r.Read(
						authProof.BitcoinSig2Bytes,
					)
					require.NoError(t, err)
				}

				numExtraBytes := r.Int31n(1000)
				if numExtraBytes > 0 {
					info.ExtraOpaqueData = make(
						[]byte, numExtraBytes,
					)
					_, err := r.Read(
						info.ExtraOpaqueData,
					)
					require.NoError(t, err)
				}

				v[0] = reflect.ValueOf(*info)
			},
		},
		{
			name: "ChannelEdgeInfo2",
			scenario: func(m models.ChannelEdgeInfo2) bool {
				return mainScenario(&m)
			},
			genValue: func(v []reflect.Value, r *rand.Rand) {
				scid := lnwire.NewShortChanIDFromInt(r.Uint64())

				chanAnn := lnwire.ChannelAnnouncement2{
					Features: *randRawFeatureVector(
						r,
					),
					ShortChannelID:  scid,
					Capacity:        r.Uint64(),
					NodeID1:         randRawKey(t),
					NodeID2:         randRawKey(t),
					ExtraOpaqueData: make([]byte, 0),
				}

				_, err := r.Read(chanAnn.ChainHash[:])
				require.NoError(t, err)

				if r.Intn(2)%2 == 0 {
					chanAnn.BitcoinKey1 = fn.Some(
						randRawKey(t),
					)
					chanAnn.BitcoinKey2 = fn.Some(
						randRawKey(t),
					)
				}
				if r.Intn(2)%2 == 0 {
					var merkleRootHash [32]byte
					_, err := r.Read(merkleRootHash[:])
					require.NoError(t, err)

					chanAnn.MerkleRootHash = fn.Some(
						merkleRootHash,
					)
				}

				numExtraBytes := r.Int31n(1000)
				if numExtraBytes > 0 {
					chanAnn.ExtraOpaqueData = make(
						[]byte, numExtraBytes,
					)
					_, err := r.Read(
						chanAnn.ExtraOpaqueData[:],
					)
					require.NoError(t, err)
				}

				info := &models.ChannelEdgeInfo2{
					ChannelAnnouncement2: chanAnn,
					ChannelPoint: wire.OutPoint{
						Index: r.Uint32(),
					},
					AuthProof: nil,
				}

				_, err = r.Read(info.ChannelPoint.Hash[:])
				require.NoError(t, err)

				if r.Intn(2)%2 == 0 {
					authProof := &models.ChannelAuthProof2{
						SchnorrSigBytes: testSchnorrSigStr, //nolint:lll
					}

					info.AuthProof = authProof
				}

				v[0] = reflect.ValueOf(*info)
			},
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			config := &quick.Config{
				Values: test.genValue,
			}

			err := quick.Check(test.scenario, config)
			require.NoError(t, err)
		})
	}
}

func randRawKey(t *testing.T) [33]byte {
	var n [33]byte

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	copy(n[:], priv.PubKey().SerializeCompressed())

	return n
}

func randRawFeatureVector(r *rand.Rand) *lnwire.RawFeatureVector {
	featureVec := lnwire.NewRawFeatureVector()
	for i := 0; i < 10000; i++ {
		if r.Int31n(2) == 0 {
			featureVec.Set(lnwire.FeatureBit(i))
		}
	}

	return featureVec
}
