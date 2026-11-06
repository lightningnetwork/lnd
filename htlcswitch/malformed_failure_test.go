package htlcswitch

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func TestMalformedFailureEncoding(t *testing.T) {
	codes := []lnwire.FailCode{
		lnwire.CodeInvalidOnionVersion, lnwire.CodeInvalidOnionHmac,
		lnwire.CodeInvalidOnionKey, lnwire.CodeInvalidBlinding,
		lnwire.FlagBadOnion | 0x123,
	}
	for _, code := range codes {
		t.Run(code.String(), func(t *testing.T) {
			msg := &lnwire.UpdateFailMalformedHTLC{
				FailureCode: code,
			}
			for i := range msg.ShaOnionBlob {
				msg.ShaOnionBlob[i] = byte(i + 1)
			}
			failure := malformedFailure(msg)
			require.Equal(t, code, failure.Code())
			var encoded bytes.Buffer
			require.NoError(
				t, lnwire.EncodeFailure(&encoded, failure, 0),
			)
			require.Len(
				t, encoded.Bytes(),
				lnwire.FailureMessageLength+4,
			)
			require.Equal(
				t, uint16(34),
				binary.BigEndian.Uint16(encoded.Bytes()[:2]),
			)
			require.Equal(
				t, uint16(code),
				binary.BigEndian.Uint16(encoded.Bytes()[2:4]),
			)
			require.Equal(
				t, msg.ShaOnionBlob[:], encoded.Bytes()[4:36],
			)
		})
	}
}

func TestMalformedFailureWithoutBadOnionFailsChannel(t *testing.T) {
	h, err := newSingleLinkTestHarness(t, btcutil.Amount(1000000), 0)
	require.NoError(t, err)
	link, ok := h.aliceLink.(*channelLink)
	require.True(t, ok)
	var got *LinkFailureError
	link.cfg.OnChannelFailure = func(_ lnwire.ChannelID,
		_ lnwire.ShortChannelID, failure LinkFailureError) {

		got = &failure
	}
	err = link.processRemoteUpdateFailMalformedHTLC(
		&lnwire.UpdateFailMalformedHTLC{FailureCode: 0x123},
	)
	require.Error(t, err)
	require.True(t, link.failed)
	require.NotNil(t, got)
	require.Equal(t, ErrInvalidUpdate, got.code)
	require.Equal(t, LinkFailureForceClose, got.FailureAction)
}
