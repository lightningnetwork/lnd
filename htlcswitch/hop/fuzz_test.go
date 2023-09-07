package hop

import (
	"bytes"
	"errors"
	"testing"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/routing/route"
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

func hopFromPayload(p *Payload) (*route.Hop, uint64) {
	return &route.Hop{
		AmtToForward:     p.FwdInfo.AmountToForward,
		OutgoingTimeLock: p.FwdInfo.OutgoingCTLV,
		MPP:              p.MPP,
		AMP:              p.AMP,
		Metadata:         p.metadata,
		CustomRecords:    p.customRecords,
	}, p.FwdInfo.NextHop.ToUint64()
}

func FuzzPayload(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > sphinx.MaxPayloadSize {
			return
		}

		r := bytes.NewReader(data)

		payload1, err := NewPayloadFromReader(r)
		if err != nil {
			return
		}

		var b bytes.Buffer
		hop, nextChanID := hopFromPayload(payload1)
		err = hop.PackHopPayload(&b, nextChanID)
		if errors.Is(err, route.ErrAMPMissingMPP) {
			// PackHopPayload refuses to encode an AMP record
			// without an MPP record. However, NewPayloadFromReader
			// does allow decoding an AMP record without an MPP
			// record, since validation is done at a later stage. Do
			// not report a bug for this case.
			return
		}
		require.NoError(t, err)

		payload2, err := NewPayloadFromReader(&b)
		require.NoError(t, err)

		require.Equal(t, payload1, payload2)
	})
}
