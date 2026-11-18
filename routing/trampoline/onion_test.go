package trampoline

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/stretchr/testify/require"
)

const (
	officialInnerOnion = "0002531fe6068134503d2723133227c867ac8fa6c8" +
		"3c537e9a44c3c5bdbdcb1fe3371860c0749bfd613056cfc5718beecc25a" +
		"2f255fc7abbea3cd75ff820e9d30807d19b30f33626452fa54bb2d822e9" +
		"18558ed3e6714deb3f9a2a10895e7553c6f088c9a852043530dbc9abcc4" +
		"86030894364b205f5de60171b451ff462664ebce23b672579bf2a444ebfe" +
		"0a81875c26d2fa16d426795b9b02ccbc4bdf909c583f0c2ebe913651064" +
		"5917153ecb05181ca0c1b207824578ee841804a148f4c3df7306dcea52d9" +
		"4222907c9187bc31c0880fc084f0d88716e195c0abe7672d15217623"

	officialNextInnerOnion = "00035e5c85814fdb522b4efeef99b44fe8a5d3d34" +
		"12057bc213b98d6f605edb022c2ae4a9141f6ac403790afeed975061f024" +
		"e2723d485f9cb35a3eaf881732f468dc19009bf195b561590798fb895b7b" +
		"7065b5537018dec330e509e8618700c9c6e1df5d15b900ac3c34104b6ab" +
		"b1099fd2eca3b640d7d5fda9370e20c09035168fc64d954baa80361b9653" +
		"14c400da2d7a64d0536bf9e494aebb80aec358327a4a1a667fcff1daf241" +
		"c99dd8c4fa907de5b931fb9daed083c157f5ea1dd960d142952f8ebe4e1" +
		"ccaee4d565a093e2b91f94b04a884ce2e8c60aced3565e8d2d10de5"
)

// TestDecodeOfficialTrampolineOnion verifies the cleartext BOLT 11 inner
// onion from lightning/bolts#836 at commit 644b82f1.
func TestDecodeOfficialTrampolineOnion(t *testing.T) {
	t.Parallel()

	associatedData := bytes.Repeat([]byte{0x42}, 32)
	packet := mustDecodeOnion(t, officialInnerOnion)

	carolKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x43}, 32))
	carolRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: carolKey},
		sphinx.NewNoOpReplayLog(),
	)

	processed, err := carolRouter.ProcessOnionPacket(
		packet, associatedData, 0,
	)
	require.NoError(t, err)

	carolPayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(100_000_000), *carolPayload.AmountToForward)
	require.Equal(t, uint32(800_000), *carolPayload.OutgoingCLTV)
	require.Equal(
		t,
		"02edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d7"+
			"c89c8f221145",
		hex.EncodeToString(
			carolPayload.OutgoingNodeID.SerializeCompressed(),
		),
	)

	var next bytes.Buffer
	require.NoError(t, processed.NextPacket.Encode(&next))
	nextHex := hex.EncodeToString(next.Bytes())
	require.Equal(t, officialNextInnerOnion, nextHex)

	eveKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x45}, 32))
	eveRouter := sphinx.NewRouter(
		&sphinx.PrivKeyECDH{PrivKey: eveKey},
		sphinx.NewNoOpReplayLog(),
	)

	processed, err = eveRouter.ProcessOnionPacket(
		processed.NextPacket, associatedData, 0,
	)
	require.NoError(t, err)

	evePayload, _, err := DecodePayload(bytes.NewReader(
		processed.Payload.Payload,
	))
	require.NoError(t, err)
	require.Equal(t, uint64(100_000_000), *evePayload.AmountToForward)
	require.Equal(t, uint32(800_000), *evePayload.OutgoingCLTV)
	require.Nil(t, evePayload.OutgoingNodeID)
	paymentAddr := evePayload.PaymentData.PaymentAddr()
	require.Equal(
		t, bytes.Repeat([]byte{0x2a}, 32),
		paymentAddr[:],
	)
	require.Equal(
		t, uint64(100_000_000),
		uint64(evePayload.PaymentData.TotalMsat()),
	)
}

// TestOnionPacketBounds verifies that malformed and oversized packets fail
// before the Sphinx decoder allocates routing information.
func TestOnionPacketBounds(t *testing.T) {
	t.Parallel()

	_, err := DecodeOnionPacket(make([]byte, MinOnionPacketSize-1))
	require.ErrorIs(t, err, ErrOnionPacketTooSmall)

	_, err = DecodeOnionPacket(make([]byte, MaxOnionPacketSize+1))
	require.ErrorIs(t, err, ErrOnionPacketTooLarge)

	invalidVersion := make([]byte, MinOnionPacketSize)
	invalidVersion[0] = 1
	_, err = DecodeOnionPacket(invalidVersion)
	require.ErrorIs(t, err, sphinx.ErrInvalidOnionVersion)
}

// TestOnionPacketRoundTrip verifies variable routing-info lengths survive a
// complete encode and decode cycle.
func TestOnionPacketRoundTrip(t *testing.T) {
	t.Parallel()

	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x31}, 32))
	packet := &sphinx.OnionPacket{
		Version:      0,
		EphemeralKey: key.PubKey(),
		RoutingInfo:  bytes.Repeat([]byte{0x17}, 97),
	}
	copy(packet.HeaderMAC[:], bytes.Repeat([]byte{0x29}, sphinx.HMACSize))

	encoded, err := EncodeOnionPacket(packet)
	require.NoError(t, err)

	decoded, err := DecodeOnionPacket(encoded)
	require.NoError(t, err)

	reencoded, err := EncodeOnionPacket(decoded)
	require.NoError(t, err)
	require.Equal(t, encoded, reencoded)
}

// TestOnionPacketRecordRoundTrip verifies the public type-20 record adapter
// preserves a variable-sized Sphinx packet.
func TestOnionPacketRecordRoundTrip(t *testing.T) {
	t.Parallel()

	packet := mustDecodeOnion(t, officialInnerOnion)
	record := NewOnionPacketRecord(&packet)
	var encoded bytes.Buffer
	require.NoError(t, record.Encode(&encoded))

	var decoded *sphinx.OnionPacket
	record = NewOnionPacketRecord(&decoded)
	require.NoError(t, record.Decode(
		bytes.NewReader(encoded.Bytes()), uint64(encoded.Len()),
	))

	want, err := EncodeOnionPacket(packet)
	require.NoError(t, err)
	got, err := EncodeOnionPacket(decoded)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// FuzzDecodeOnionPacket exercises the complete untrusted variable-sized onion
// parser and asserts successful parses have a stable encoding.
func FuzzDecodeOnionPacket(f *testing.F) {
	f.Add(mustHex(f, officialInnerOnion))
	f.Add(make([]byte, 0))
	f.Add(make([]byte, MinOnionPacketSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		packet, err := DecodeOnionPacket(data)
		if err != nil {
			return
		}

		encoded, err := EncodeOnionPacket(packet)
		require.NoError(t, err)
		require.Equal(t, data, encoded)
	})
}

func mustDecodeOnion(t testing.TB, value string) *sphinx.OnionPacket {
	t.Helper()

	packet, err := DecodeOnionPacket(mustHex(t, value))
	require.NoError(t, err)

	return packet
}

func mustHex(t testing.TB, value string) []byte {
	t.Helper()

	value = strings.ReplaceAll(value, " ", "")
	decoded, err := hex.DecodeString(value)
	require.NoError(t, err)

	return decoded
}
