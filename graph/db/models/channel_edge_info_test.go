package models

import (
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
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
