package hop

import (
	"bytes"
	"errors"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
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
		EncryptedData:    p.encryptedData,
		BlindingPoint:    p.blindingPoint,
		CustomRecords:    p.customRecords,
		TotalAmtMsat:     p.totalAmtMsat,
	}, p.FwdInfo.NextHop.ToUint64()
}

// FuzzPayloadFinal fuzzes final hop payloads, providing the additional context
// that the hop should be final (which is usually obtained by the structure
// of the sphinx packet).
func FuzzPayloadFinal(f *testing.F) {
	fuzzPayload(f, true)
}

// FuzzPayloadIntermediate fuzzes intermediate hop payloads, providing the
// additional context that a hop should be intermediate (which is usually
// obtained by the structure of the sphinx packet).
func FuzzPayloadIntermediate(f *testing.F) {
	fuzzPayload(f, false)
}

func fuzzPayload(f *testing.F, finalPayload bool) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > sphinx.MaxPayloadSize {
			return
		}

		r := bytes.NewReader(data)

		payload1, err := NewPayloadFromReader(
			r, finalPayload, &BlindingKit{
				ForwardingInfo: func(*btcec.PublicKey,
					[]byte) (*ForwardingInfo, error) {

					return &ForwardingInfo{}, nil
				},
			},
		)
		if err != nil {
			return
		}

		var b bytes.Buffer
		hop, nextChanID := hopFromPayload(payload1)
		err = hop.PackHopPayload(&b, nextChanID, finalPayload)
		switch {
		// PackHopPayload refuses to encode an AMP record
		// without an MPP record. However, NewPayloadFromReader
		// does allow decoding an AMP record without an MPP
		// record, since validation is done at a later stage. Do
		// not report a bug for this case.
		case errors.Is(err, route.ErrAMPMissingMPP):
			return

		// PackHopPayload will not encode regular payloads or final
		// hops in blinded routes that do not have an amount or expiry
		// TLV set. However, NewPayloadFromReader will allow creation
		// of payloads where these TLVs are present, but they have
		// zero values because validation is done at a later stage.
		case errors.Is(err, route.ErrMissingField):
			return

		default:
			require.NoError(t, err)
		}

		payload2, err := NewPayloadFromReader(
			&b, finalPayload, &BlindingKit{
				ForwardingInfo: func(*btcec.PublicKey,
					[]byte) (*ForwardingInfo, error) {

					return &ForwardingInfo{}, nil
				},
			},
		)
		require.NoError(t, err)

		require.Equal(t, payload1, payload2)
	})
}
