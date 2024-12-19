package lnwire

import (
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// TestChannelReadyStringProperty tests the string property of the ChannelReady
// message.
func TestChannelReadyStringProperty(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(t *rapid.T) {
		// Generate random channel ID.
		chanID := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			t, "channel_id",
		)

		// Generate random public key.
		privKey := rapid.SliceOfN(rapid.Byte(), 32, 32).Draw(
			t, "priv_key",
		)
		_, pubKey := btcec.PrivKeyFromBytes(privKey)

		// Randomly decide if we should have an alias SCID.
		hasAlias := rapid.Bool().Draw(t, "has_alias")

		// Create the channel ready message.
		msg := &ChannelReady{
			ChanID:                 ChannelID(chanID),
			NextPerCommitmentPoint: pubKey,
		}

		if hasAlias {
			// Generate random alias SCID.
			aliasID := rapid.Uint64().Draw(t, "alias_id")
			alias := NewShortChanIDFromInt(aliasID)
			msg.AliasScid = &alias

			// When we have an alias, verify it appears in the
			// string.
			str := msg.String()
			require.Contains(
				t, str, fmt.Sprintf("uint=%d", aliasID),
			)
		} else {
			// When we don't have an alias, verify it shows as nil.
			str := msg.String()
			require.Contains(t, str, "aliasSCID=nil")
		}

		// Verify the channel ID appears in the string.
		str := msg.String()
		require.Contains(t, str, msg.ChanID.String())

		// Verify the public key appears in the string
		pubKeyHex := hex.EncodeToString(
			msg.NextPerCommitmentPoint.SerializeCompressed(),
		)
		require.Contains(t, str, pubKeyHex)
	})
}
