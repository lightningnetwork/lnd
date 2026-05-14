package channeldb

import (
	"bytes"
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
)

// TestWriteElementSingleAddrRejectsV2 ensures that the scalar net.Addr write
// path returns an explicit error when handed an unserializable address (here,
// a legacy v2 onion) instead of silently emitting zero bytes. A silent skip
// would corrupt the byte stream because the matching ReadElement(*net.Addr)
// case has no length prefix to recover from.
func TestWriteElementSingleAddrRejectsV2(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		// 16 base32 chars + ".onion" = 22 chars = tor.V2Len.
		OnionService: "aaaaaaaaaaaaaaaa.onion",
		Port:         9735,
	}
	require.Len(t, v2.OnionService, tor.V2Len)

	var buf bytes.Buffer
	err := WriteElement(&buf, net.Addr(v2))
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported address")
	require.Zero(
		t, buf.Len(),
		"no bytes should be written when serialization is refused",
	)
}

// TestWriteElementSingleAddrRoundTrip confirms that supported addresses still
// round-trip through the scalar net.Addr path after the v2 rejection change.
func TestWriteElementSingleAddrRoundTrip(t *testing.T) {
	t.Parallel()

	want := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735}

	var buf bytes.Buffer
	require.NoError(t, WriteElement(&buf, net.Addr(want)))

	var got net.Addr
	require.NoError(t, ReadElement(&buf, &got))
	require.Equal(t, want.String(), got.String())
}

// TestWriteElementAddrSliceDropsV2 documents that the length-prefixed
// []net.Addr path still filters v2 silently. The leading count is rewritten
// from the filtered slice, so the reader stays in sync and the stream is not
// corrupted.
func TestWriteElementAddrSliceDropsV2(t *testing.T) {
	t.Parallel()

	v2 := &tor.OnionAddr{
		OnionService: "aaaaaaaaaaaaaaaa.onion",
		Port:         9735,
	}
	tcp := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9735}

	var buf bytes.Buffer
	require.NoError(t, WriteElement(&buf, []net.Addr{v2, tcp}))

	var got []net.Addr
	require.NoError(t, ReadElement(&buf, &got))
	require.Len(t, got, 1)
	require.Equal(t, tcp.String(), got[0].String())

	// Sanity check that the v2-only slice round-trips as an empty slice
	// rather than producing an error.
	buf.Reset()
	require.NoError(t, WriteElement(&buf, []net.Addr{v2}))

	got = nil
	require.NoError(t, ReadElement(&buf, &got))
	require.Empty(t, got)
}
