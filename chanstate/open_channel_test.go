package chanstate

import (
	"testing"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestActiveHtlcsMatchesByHTLCIdentity asserts that ActiveHtlcs matches HTLCs
// by their channel identity, not by their onion blob. Onion blobs are routing
// payload data and can be duplicated, while the HTLC index plus direction
// identifies an offered HTLC within the channel state.
func TestActiveHtlcsMatchesByHTLCIdentity(t *testing.T) {
	t.Parallel()

	var onionBlob [lnwire.OnionPacketSize]byte
	onionBlob[0] = 1

	matchingHTLC := HTLC{
		HtlcIndex: 7,
		LogIndex:  10,
		Incoming:  false,
		OnionBlob: onionBlob,
	}
	duplicateOnionHTLC := HTLC{
		HtlcIndex: 8,
		LogIndex:  11,
		Incoming:  false,
		OnionBlob: onionBlob,
	}
	oppositeDirectionHTLC := HTLC{
		HtlcIndex: 7,
		LogIndex:  12,
		Incoming:  true,
		OnionBlob: onionBlob,
	}

	channel := &OpenChannel{
		LocalCommitment: ChannelCommitment{
			Htlcs: []HTLC{
				matchingHTLC,
				duplicateOnionHTLC,
				oppositeDirectionHTLC,
			},
		},
		RemoteCommitment: ChannelCommitment{
			Htlcs: []HTLC{
				matchingHTLC,
			},
		},
	}

	activeHtlcs := channel.ActiveHtlcs()
	require.Len(t, activeHtlcs, 1)
	require.Equal(t, matchingHTLC.HtlcIndex, activeHtlcs[0].HtlcIndex)
	require.Equal(t, matchingHTLC.Incoming, activeHtlcs[0].Incoming)
}
