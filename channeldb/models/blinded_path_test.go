package models

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/stretchr/testify/require"
)

func genPubKey(t *testing.T) route.Vertex {
	pk, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return route.NewVertex(pk.PubKey())
}

func genPrivKey(t *testing.T) *btcec.PrivateKey {
	pk, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	return pk
}

// TestBlindedPathInfoSerialise tests the BlindedPathInfo TLV encoder and
// decoder.
func TestBlindedPathInfoSerialise(t *testing.T) {
	info := &BlindedPathInfo{
		Route: &MCRoute{
			SourcePubKey: genPubKey(t),
			TotalAmount:  600,
			Hops: []*MCHop{
				{
					PubKeyBytes: genPubKey(t),
					AmtToFwd:    60,
					ChannelID:   40,
				},
				{
					PubKeyBytes:      genPubKey(t),
					AmtToFwd:         50,
					ChannelID:        10,
					HasBlindingPoint: true,
				},
			},
		},
		SessionKey: genPrivKey(t),
	}

	var b bytes.Buffer
	err := info.Serialize(&b)
	require.NoError(t, err)

	info2, err := DeserializeBlindedPathInfo(&b)
	require.NoError(t, err)

	compareBlindedPathInfo(t, info, info2)

	infoSet := &BlindedPathsInfo{
		genPubKey(t): &BlindedPathInfo{
			Route: &MCRoute{
				SourcePubKey: genPubKey(t),
				TotalAmount:  600,
				Hops: []*MCHop{
					{
						PubKeyBytes: genPubKey(t),
						AmtToFwd:    60,
						ChannelID:   40,
					},
					{
						PubKeyBytes:      genPubKey(t),
						AmtToFwd:         50,
						ChannelID:        10,
						HasBlindingPoint: true,
					},
				},
			},
			SessionKey: genPrivKey(t),
		},
		genPubKey(t): &BlindedPathInfo{
			Route: &MCRoute{
				SourcePubKey: genPubKey(t),
				TotalAmount:  60,
				Hops: []*MCHop{
					{
						PubKeyBytes: genPubKey(t),
						AmtToFwd:    60,
						ChannelID:   400,
					},
				},
			},
			SessionKey: genPrivKey(t),
		},
	}

	var b1 bytes.Buffer
	err = BlindedPathInfoEncoder(&b1, infoSet, nil)
	require.NoError(t, err)

	infoSet2 := make(BlindedPathsInfo)
	err = BlindedPathInfoDecoder(
		&b1, &infoSet2, nil, uint64(len(b1.Bytes())),
	)
	require.NoError(t, err)
}

func compareBlindedPathInfo(t *testing.T, info, info2 *BlindedPathInfo) {
	require.EqualValues(t, info.SessionKey, info2.SessionKey)
	require.EqualValues(
		t, info.Route.SourcePubKey, info2.Route.SourcePubKey,
	)
	require.EqualValues(
		t, info.Route.TotalAmount, info2.Route.TotalAmount,
	)
	require.Len(t, info.Route.Hops, len(info2.Route.Hops))

	for i, hop := range info.Route.Hops {
		hop2 := info2.Route.Hops[i]
		require.EqualValues(t, hop.PubKeyBytes, hop2.PubKeyBytes)
		require.EqualValues(t, hop.ChannelID, hop2.ChannelID)
		require.EqualValues(
			t, hop.HasBlindingPoint, hop2.HasBlindingPoint,
		)
		require.EqualValues(t, hop.AmtToFwd, hop2.AmtToFwd)
	}
}
