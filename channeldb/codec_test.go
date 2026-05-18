package channeldb

import (
	"bytes"
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestWriteElementSingleAddrRoundTrip ensures the scalar net.Addr write path
// round-trips a supported address through the channeldb codec.
func TestWriteElementSingleAddrRoundTrip(t *testing.T) {
	t.Parallel()

	want := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735}

	var buf bytes.Buffer
	require.NoError(t, WriteElement(&buf, net.Addr(want)))

	var got net.Addr
	require.NoError(t, ReadElement(&buf, &got))
	require.Equal(t, want.String(), got.String())
}

// TestWriteElementAddrSlicePreservesV2 verifies that the length-prefixed
// []net.Addr write path persists legacy v2 onion entries instead of dropping
// them. Storage must round-trip the exact address set a peer signed, so v2 is
// kept on disk and filtered only at consumption time (dial logic, RPC).
func TestWriteElementAddrSlicePreservesV2(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		// 16 base32 chars + ".onion" = 22 chars = tor.V2Len.
		OnionService: "abcdefghijklmnop.onion",
		Port:         9735,
	}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735}

	var buf bytes.Buffer
	require.NoError(t, WriteElement(&buf, []net.Addr{v2, tcp}))

	var got []net.Addr
	require.NoError(t, ReadElement(&buf, &got))
	require.Len(t, got, 2)
	require.Equal(t, v2.String(), got[0].String())
	require.Equal(t, tcp.String(), got[1].String())
}
