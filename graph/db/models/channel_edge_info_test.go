package models

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestFundingPKScriptV2 tests the FundingPKScript method for v2 channels
// which uses MuSig2 key aggregation.
func TestFundingPKScriptV2(t *testing.T) {
	t.Parallel()

	// Generate two test keys for bitcoin keys.
	privKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey1 := privKey1.PubKey()

	privKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey2 := privKey2.PubKey()

	// Convert to route.Vertex format.
	var btcKey1, btcKey2 route.Vertex
	copy(btcKey1[:], pubKey1.SerializeCompressed())
	copy(btcKey2[:], pubKey2.SerializeCompressed())

	// Create a test merkle root.
	var merkleRoot chainhash.Hash
	copy(merkleRoot[:], []byte("test-merkle-root-hash-32-bytes!"))

	t.Run("v2 with btc keys, no merkle root (BIP86)", func(t *testing.T) {
		t.Parallel()

		edge := &ChannelEdgeInfo{
			Version:          lnwire.GossipVersion2,
			BitcoinKey1Bytes: fn.Some(btcKey1),
			BitcoinKey2Bytes: fn.Some(btcKey2),
		}

		pkScript, err := edge.FundingPKScript()
		require.NoError(t, err)
		require.NotEmpty(t, pkScript)

		// Verify it's a valid taproot script (OP_1 <32-byte-key>).
		require.Len(t, pkScript, 34)
		require.Equal(t, byte(0x51), pkScript[0]) // OP_1

		// Manually compute expected script using BIP86 tweak.
		combinedKey, _, _, err := musig2.AggregateKeys(
			[]*btcec.PublicKey{pubKey1, pubKey2}, true,
			musig2.WithBIP86KeyTweak(),
		)
		require.NoError(t, err)

		expectedScript, err := input.PayToTaprootScript(
			combinedKey.FinalKey,
		)
		require.NoError(t, err)
		require.Equal(t, expectedScript, pkScript)
	})

	t.Run("v2 with bitcoin keys and merkle root", func(t *testing.T) {
		t.Parallel()

		edge := &ChannelEdgeInfo{
			Version:          lnwire.GossipVersion2,
			BitcoinKey1Bytes: fn.Some(btcKey1),
			BitcoinKey2Bytes: fn.Some(btcKey2),
			MerkleRootHash:   fn.Some(merkleRoot),
		}

		pkScript, err := edge.FundingPKScript()
		require.NoError(t, err)
		require.NotEmpty(t, pkScript)

		// Verify it's a valid taproot script.
		require.Len(t, pkScript, 34)
		require.Equal(t, byte(0x51), pkScript[0]) // OP_1

		// Manually compute expected script with taproot tweak.
		combinedKey, _, _, err := musig2.AggregateKeys(
			[]*btcec.PublicKey{pubKey1, pubKey2}, true,
			musig2.WithTaprootKeyTweak(merkleRoot[:]),
		)
		require.NoError(t, err)

		expectedScript, err := input.PayToTaprootScript(
			combinedKey.FinalKey,
		)
		require.NoError(t, err)
		require.Equal(t, expectedScript, pkScript)
	})

	t.Run("v2 no btc keys returns stored script", func(t *testing.T) {
		t.Parallel()

		storedScript := []byte{0x51, 0x20} // OP_1 + push 32
		storedScript = append(storedScript, make([]byte, 32)...)

		edge := &ChannelEdgeInfo{
			Version:       lnwire.GossipVersion2,
			FundingScript: fn.Some(storedScript),
		}

		pkScript, err := edge.FundingPKScript()
		require.NoError(t, err)
		require.Equal(t, storedScript, pkScript)
	})

	t.Run("v2 no btc keys and no stored script errors", func(t *testing.T) {
		t.Parallel()

		edge := &ChannelEdgeInfo{
			Version: lnwire.GossipVersion2,
		}

		_, err := edge.FundingPKScript()
		require.Error(t, err)
		require.Contains(t, err.Error(), "expected a funding pk script")
	})

	t.Run("v2 one btc key returns stored script", func(t *testing.T) {
		t.Parallel()

		storedScript := []byte{0x51, 0x20}
		storedScript = append(storedScript, make([]byte, 32)...)

		// Only key1 set, key2 missing.
		edge := &ChannelEdgeInfo{
			Version:          lnwire.GossipVersion2,
			BitcoinKey1Bytes: fn.Some(btcKey1),
			FundingScript:    fn.Some(storedScript),
		}

		pkScript, err := edge.FundingPKScript()
		require.NoError(t, err)
		require.Equal(t, storedScript, pkScript)
	})
}

// TestFundingPKScriptV1TaprootFeatureBit tests that a v1 channel edge carrying
// the taproot staging bit reconstructs a taproot funding script.
func TestFundingPKScriptV1TaprootFeatureBit(t *testing.T) {
	t.Parallel()

	privKey1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey1 := privKey1.PubKey()

	privKey2, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKey2 := privKey2.PubKey()

	var btcKey1, btcKey2 route.Vertex
	copy(btcKey1[:], pubKey1.SerializeCompressed())
	copy(btcKey2[:], pubKey2.SerializeCompressed())

	// Build a v1 edge that only has the legacy bitcoin keys populated, but
	// does advertise the taproot staging bit in its feature vector.
	edge := &ChannelEdgeInfo{
		Version:          lnwire.GossipVersion1,
		BitcoinKey1Bytes: fn.Some(btcKey1),
		BitcoinKey2Bytes: fn.Some(btcKey2),
		Features: lnwire.NewFeatureVector(
			lnwire.NewRawFeatureVector(
				lnwire.SimpleTaprootChannelsRequiredStaging,
			),
			lnwire.Features,
		),
	}

	pkScript, err := edge.FundingPKScript()
	require.NoError(t, err)

	// The fix should make FundingPKScript honor the feature bit and derive
	// the taproot funding script directly from the stored bitcoin keys.
	expectedScript, _, err := input.GenTaprootFundingScript(
		pubKey1, pubKey2, 0, fn.None[chainhash.Hash](),
	)
	require.NoError(t, err)

	require.Equal(t, expectedScript, pkScript)
}

// TestChannelAnnouncementWireRoundTrip asserts that converting a channel
// announcement into a ChannelEdgeInfo and back reproduces the exact bytes that
// the sender signed, for both gossip versions.
//
// This matters because lnd stores announcements as ChannelEdgeInfo records and
// later rebuilds the wire message from them, for example to send it to a peer.
// The signature covers the encoded message, so a rebuilt announcement only
// verifies if it encodes to the same bytes as the original.
//
// A v2 announcement makes its chain hash and features records optional: when
// absent, they mean bitcoin mainnet and no features. The model doesn't record
// whether the sender included them, so ToWireAnnouncement uses the minimal
// encoding and omits them when they have their default value. The v2 cases
// below cover both the minimal encoding and an announcement that carries the
// records with non-default values.
func TestChannelAnnouncementWireRoundTrip(t *testing.T) {
	t.Parallel()

	// newVertex returns the compressed public key of a fresh private key.
	// The node and bitcoin keys of the announcements only need to be
	// distinct valid keys, so each call returns a new one.
	newVertex := func() [33]byte {
		key, err := btcec.NewPrivateKey()
		require.NoError(t, err)

		var vertex [33]byte
		copy(vertex[:], key.PubKey().SerializeCompressed())

		return vertex
	}

	// Create real signatures for the announcements. The test doesn't
	// verify them, but the conversion parses them into their stored form
	// and back, which only works for well-formed signatures: an ECDSA
	// signature for v1, which has four of them, and a Schnorr signature for
	// v2, which has a single one.
	signKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	hash := chainhash.HashH([]byte("channel announcement"))

	ecdsaSig, err := lnwire.NewSigFromSignature(
		ecdsa.Sign(signKey, hash[:]),
	)
	require.NoError(t, err)

	schnorrSig, err := schnorr.Sign(signKey, hash[:])
	require.NoError(t, err)
	v2Sig, err := lnwire.NewSigFromSchnorrRawSignature(
		schnorrSig.Serialize(),
	)
	require.NoError(t, err)

	// roundTrip performs the conversion under test. It first extracts the
	// auth proof from the announcement, then builds the ChannelEdgeInfo
	// that lnd would store from the announcement and that proof, and
	// finally rebuilds a wire announcement from the stored record. It
	// asserts that the original and the rebuilt announcement encode to the
	// same bytes, and returns the record so that callers can check how the
	// announcement's fields were stored.
	roundTrip := func(t *testing.T,
		ann lnwire.ChannelAnnouncement) *ChannelEdgeInfo {

		t.Helper()

		proof, err := ChannelAuthProofFromWireAnnouncement(ann)
		require.NoError(t, err)

		info, err := ChannelEdgeInfoFromWireAnnouncement(ann, proof)
		require.NoError(t, err)

		rebuilt, err := info.ToWireAnnouncement()
		require.NoError(t, err)

		var want, got bytes.Buffer
		require.NoError(t, ann.Encode(&want, 0))
		require.NoError(t, rebuilt.Encode(&got, 0))
		require.Equal(t, want.Bytes(), got.Bytes())

		return info
	}

	// A v1 announcement has no optional fields: every field is always
	// encoded. It uses a testnet chain hash and a feature bit, so that a
	// conversion that dropped or defaulted either of them would change the
	// encoding.
	t.Run("v1", func(t *testing.T) {
		t.Parallel()

		ann := &lnwire.ChannelAnnouncement1{
			NodeSig1:    ecdsaSig,
			NodeSig2:    ecdsaSig,
			BitcoinSig1: ecdsaSig,
			BitcoinSig2: ecdsaSig,
			Features: lnwire.NewRawFeatureVector(
				lnwire.GossipQueriesOptional,
			),
			ChainHash:      *chaincfg.TestNet3Params.GenesisHash,
			ShortChannelID: lnwire.NewShortChanIDFromInt(42),
			NodeID1:        newVertex(),
			NodeID2:        newVertex(),
			BitcoinKey1:    newVertex(),
			BitcoinKey2:    newVertex(),
		}

		info := roundTrip(t, ann)
		require.Equal(t, lnwire.GossipVersion1, info.Version)
	})

	// newV2Ann returns a v2 announcement with all of its required records
	// set, and with the optional chain hash and features records left
	// out, which is how a sender on bitcoin mainnet without features
	// encodes it. It also sets the optional bitcoin keys and an unknown
	// signed field, so that the round trip covers optional records and
	// the extra signed fields too. The cases below add the remaining
	// optional records where they need them.
	newV2Ann := func() *lnwire.ChannelAnnouncement2 {
		ann := &lnwire.ChannelAnnouncement2{
			ExtraSignedFields: lnwire.ExtraSignedFields{
				101: {0x01, 0x02},
			},
		}
		ann.ShortChannelID.Val = lnwire.NewShortChanIDFromInt(42)
		ann.Capacity.Val = 100_000
		ann.NodeID1.Val = newVertex()
		ann.NodeID2.Val = newVertex()
		ann.Outpoint.Val = lnwire.OutPoint{Index: 1}
		ann.Signature.Val = v2Sig

		btcKey1 := ann.BitcoinKey1.Zero()
		btcKey1.Val = newVertex()
		ann.BitcoinKey1 = tlv.SomeRecordT(btcKey1)

		btcKey2 := ann.BitcoinKey2.Zero()
		btcKey2.Val = newVertex()
		ann.BitcoinKey2 = tlv.SomeRecordT(btcKey2)

		return ann
	}

	// The minimal encoding: no chain hash and no features. Rebuilding it
	// must leave both records out again, since encoding them, even with
	// their default values, would change the signed bytes. The stored
	// record must also hold the defaults that the absent records stand
	// for.
	t.Run("v2 on mainnet without features", func(t *testing.T) {
		t.Parallel()

		info := roundTrip(t, newV2Ann())
		require.Equal(t, lnwire.GossipVersion2, info.Version)

		require.Equal(
			t, *chaincfg.MainNetParams.GenesisHash, info.ChainHash,
		)
		require.True(t, info.Features.IsEmpty())
	})

	// An announcement that carries the optional records with non-default
	// values: a testnet chain hash, a feature bit and a merkle root.
	// Rebuilding it must encode all three again, and the stored record
	// must hold their values rather than the defaults.
	t.Run("v2 on testnet with features", func(t *testing.T) {
		t.Parallel()

		ann := newV2Ann()

		chainHash := ann.ChainHash.Zero()
		chainHash.Val = *chaincfg.TestNet3Params.GenesisHash
		ann.ChainHash = tlv.SomeRecordT(chainHash)

		features := ann.Features.Zero()
		features.Val = *lnwire.NewRawFeatureVector(
			lnwire.GossipQueriesOptional,
		)
		ann.Features = tlv.SomeRecordT(features)

		merkleRoot := ann.MerkleRootHash.Zero()
		merkleRoot.Val = [32]byte{0x01}
		ann.MerkleRootHash = tlv.SomeRecordT(merkleRoot)

		info := roundTrip(t, ann)
		require.Equal(
			t, *chaincfg.TestNet3Params.GenesisHash, info.ChainHash,
		)
		require.False(t, info.Features.IsEmpty())
	})
}
