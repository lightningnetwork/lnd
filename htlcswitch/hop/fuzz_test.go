package hop

import (
	"bytes"
	"testing"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/stretchr/testify/require"
)

const (
	LegacyPayloadSize  = sphinx.LegacyHopDataSize - sphinx.HMACSize
	MaxOnionPacketSize = 1366
)

func FuzzHopData(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > LegacyPayloadSize {
			return
		}

		r := bytes.NewReader(data)

		var hopData1, hopData2 sphinx.HopData

		if err := hopData1.Decode(r); err != nil {
			return
		}

		var b bytes.Buffer
		require.NoError(t, hopData1.Encode(&b))
		require.NoError(t, hopData2.Decode(&b))

		require.Equal(t, hopData1, hopData2)
	})
}

func FuzzHopPayload(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > sphinx.MaxPayloadSize {
			return
		}

		r := bytes.NewReader(data)

		var hopPayload1, hopPayload2 sphinx.HopPayload

		if err := hopPayload1.Decode(r); err != nil {
			return
		}

		var b bytes.Buffer
		require.NoError(t, hopPayload1.Encode(&b))
		require.NoError(t, hopPayload2.Decode(&b))

		require.Equal(t, hopPayload1, hopPayload2)
	})
}

func FuzzOnionPacket(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxOnionPacketSize {
			return
		}

		r := bytes.NewReader(data)

		var pkt1, pkt2 sphinx.OnionPacket

		if err := pkt1.Decode(r); err != nil {
			return
		}

		var b bytes.Buffer
		require.NoError(t, pkt1.Encode(&b))
		require.NoError(t, pkt2.Decode(&b))

		require.Equal(t, pkt1, pkt2)
	})
}
