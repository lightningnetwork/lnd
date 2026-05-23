package lnwire

import (
	"bytes"
	"testing"

	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestSciddirIntroCodec tests the sciddir_or_pubkey codec that a
// channel_update_2 short_channel_id uses. The field is constrained to the
// sciddir form, so the decoder rejects the pubkey form, a wrong length and an
// out-of-range direction byte, and the encoder rejects the same direction.
func TestSciddirIntroCodec(t *testing.T) {
	t.Parallel()

	encode := func(t *testing.T, intro SciddirIntro) ([]byte, error) {
		t.Helper()

		var b bytes.Buffer
		err := sciddirIntroEncoder(&b, &intro, nil)

		return b.Bytes(), err
	}

	decode := func(t *testing.T, raw []byte) (SciddirIntro, error) {
		t.Helper()

		var intro SciddirIntro
		err := sciddirIntroDecoder(
			bytes.NewReader(raw), &intro, nil, uint64(len(raw)),
		)

		return intro, err
	}

	t.Run("round trip", func(t *testing.T) {
		t.Parallel()

		for _, direction := range []byte{0, 1} {
			intro := SciddirIntro{
				Direction: direction,
				SCID: [scidLen]byte{
					1, 2, 3, 4, 5, 6, 7, 8,
				},
			}

			raw, err := encode(t, intro)
			require.NoError(t, err)
			require.Len(t, raw, sciddirLen)

			got, err := decode(t, raw)
			require.NoError(t, err)
			require.Equal(t, intro, got)
		}
	})

	t.Run("encode rejects direction", func(t *testing.T) {
		t.Parallel()

		_, err := encode(t, SciddirIntro{Direction: 2})
		require.ErrorIs(t, err, ErrInvalidIntroNode)
	})

	t.Run("decode rejects direction", func(t *testing.T) {
		t.Parallel()

		raw := make([]byte, sciddirLen)
		raw[0] = 2

		_, err := decode(t, raw)
		require.ErrorIs(t, err, ErrInvalidIntroNode)
	})

	t.Run("decode rejects pubkey form", func(t *testing.T) {
		t.Parallel()

		_, err := decode(t, make([]byte, pubKeyLen))
		require.ErrorContains(t, err, "pubkey form")
	})

	t.Run("decode rejects wrong length", func(t *testing.T) {
		t.Parallel()

		rangeLen := []int{0, sciddirLen - 1, sciddirLen + 1}

		for _, length := range rangeLen {
			_, err := decode(t, make([]byte, length))
			require.ErrorAs(t, err, &tlv.ErrTypeForDecoding{})
		}
	})
}

// TestChannelUpdate2RejectsBadSciddir tests that the public ChannelUpdate2
// decoder rejects a short_channel_id that is not a well-formed sciddir.
func TestChannelUpdate2RejectsBadSciddir(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		scid []byte
	}{
		{
			name: "direction out of range",
			scid: append([]byte{2}, make([]byte, scidLen)...),
		},
		{
			name: "pubkey form",
			scid: make([]byte, pubKeyLen),
		},
		{
			name: "short",
			scid: make([]byte, sciddirLen-1),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			raw, err := EncodeRecords(tlv.MapToRecords(
				map[uint64][]byte{
					2:   test.scid,
					4:   make([]byte, 4),
					240: make([]byte, 64),
				},
			))
			require.NoError(t, err)

			var msg ChannelUpdate2
			require.Error(t, msg.Decode(bytes.NewReader(raw), 0))
		})
	}
}
