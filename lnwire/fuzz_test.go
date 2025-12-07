package lnwire

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// prefixWithMsgType takes []byte and adds a wire protocol prefix
// to make the []byte into an actual message to be used in fuzzing.
func prefixWithMsgType(data []byte, prefix MessageType) []byte {
	var prefixBytes [2]byte
	binary.BigEndian.PutUint16(prefixBytes[:], uint16(prefix))
	data = append(prefixBytes[:], data...)

	return data
}

// assertEqualFunc is a function used to assert that two deserialized messages
// are equivalent.
type assertEqualFunc func(t *testing.T, x, y any)

// wireMsgHarnessCustom performs the actual fuzz testing of the appropriate wire
// message. This function will check that the passed-in message passes wire
// length checks, is a valid message once deserialized, and passes a sequence of
// serialization and deserialization checks.
func wireMsgHarnessCustom(t *testing.T, data []byte, msgType MessageType,
	assertEqual assertEqualFunc) {

	data = prefixWithMsgType(data, msgType)

	// Create a reader with the byte array.
	r := bytes.NewReader(data)

	// Check that the created message is not greater than the maximum
	// message size.
	if len(data) > MaxSliceLength {
		return
	}

	msg, err := ReadMessage(r, 0)
	if err != nil {
		return
	}

	// We will serialize the message into a new bytes buffer.
	var b bytes.Buffer
	_, err = WriteMessage(&b, msg, 0)
	require.NoError(t, err)

	// Deserialize the message from the serialized bytes buffer, and then
	// assert that the original message is equal to the newly deserialized
	// message.
	newMsg, err := ReadMessage(&b, 0)
	require.NoError(t, err)

	assertEqual(t, msg, newMsg)
}

func wireMsgHarness(t *testing.T, data []byte, msgType MessageType) {
	t.Helper()
	assertEq := func(t *testing.T, x, y any) {
		require.Equal(t, x, y)
	}
	wireMsgHarnessCustom(t, data, msgType, assertEq)
}

func FuzzAcceptChannel(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for UpfrontShutdownScript, since
		// we consider the empty slice and nil to be equivalent.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &AcceptChannel{}, x)
			first, _ := x.(*AcceptChannel)
			require.IsType(t, &AcceptChannel{}, y)
			second, _ := y.(*AcceptChannel)

			require.True(
				t, bytes.Equal(
					first.UpfrontShutdownScript,
					second.UpfrontShutdownScript,
				),
			)
			first.UpfrontShutdownScript = nil
			second.UpfrontShutdownScript = nil

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgAcceptChannel, assertEq)
	})
}

func FuzzAnnounceSignatures(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgAnnounceSignatures)
	})
}

func FuzzAnnounceSignatures2(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgAnnounceSignatures2)
	})
}

func FuzzChannelAnnouncement(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgChannelAnnouncement)
	})
}

func FuzzChannelAnnouncement2(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for Features, since we consider
		// the empty map and nil to be equivalent.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &ChannelAnnouncement2{}, x)
			first, _ := x.(*ChannelAnnouncement2)
			require.IsType(t, &ChannelAnnouncement2{}, y)
			second, _ := y.(*ChannelAnnouncement2)

			require.True(
				t,
				first.Features.Val.Equals(&second.Features.Val),
			)
			first.Features.Val = *NewRawFeatureVector()
			second.Features.Val = *NewRawFeatureVector()

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgChannelAnnouncement2, assertEq)
	})
}

func FuzzChannelReestablish(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgChannelReestablish)
	})
}

func FuzzChannelUpdate(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgChannelUpdate)
	})
}

func FuzzChannelUpdate2(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgChannelUpdate2)
	})
}

func FuzzClosingSigned(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgClosingSigned)
	})
}

func FuzzCommitSig(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgCommitSig)
	})
}

func FuzzError(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgError)
	})
}

func FuzzWarning(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgWarning)
	})
}

func FuzzStfu(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgStfu)
	})
}

func FuzzFundingCreated(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgFundingCreated)
	})
}

func FuzzChannelReady(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgChannelReady)
	})
}

func FuzzFundingSigned(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgFundingSigned)
	})
}

func FuzzGossipTimestampRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgGossipTimestampRange)
	})
}

func FuzzInit(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgInit)
	})
}

func FuzzNodeAnnouncement(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for Addresses, since the same IP
		// can be represented by different underlying bytes. Instead, we
		// compare the normalized string representation of each address.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &NodeAnnouncement1{}, x)
			first, _ := x.(*NodeAnnouncement1)
			require.IsType(t, &NodeAnnouncement1{}, y)
			second, _ := y.(*NodeAnnouncement1)

			require.Equal(
				t, len(first.Addresses), len(second.Addresses),
			)
			for i := range first.Addresses {
				require.Equal(
					t, first.Addresses[i].String(),
					second.Addresses[i].String(),
				)
			}
			first.Addresses = nil
			second.Addresses = nil

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgNodeAnnouncement, assertEq)
	})
}

func FuzzNodeAnnouncement2(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for Features, since we consider
		// the empty map and nil to be equivalent.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &NodeAnnouncement2{}, x)
			first, _ := x.(*NodeAnnouncement2)
			require.IsType(t, &NodeAnnouncement2{}, y)
			second, _ := y.(*NodeAnnouncement2)

			require.True(
				t,
				first.Features.Val.Equals(&second.Features.Val),
			)
			first.Features.Val = *NewRawFeatureVector()
			second.Features.Val = *NewRawFeatureVector()

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgNodeAnnouncement2, assertEq)
	})
}

func FuzzOpenChannel(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for UpfrontShutdownScript, since
		// we consider the empty slice and nil to be equivalent.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &OpenChannel{}, x)
			first, _ := x.(*OpenChannel)
			require.IsType(t, &OpenChannel{}, y)
			second, _ := y.(*OpenChannel)

			require.True(
				t, bytes.Equal(
					first.UpfrontShutdownScript,
					second.UpfrontShutdownScript,
				),
			)
			first.UpfrontShutdownScript = nil
			second.UpfrontShutdownScript = nil

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgOpenChannel, assertEq)
	})
}

func FuzzPing(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgPing)
	})
}

func FuzzPong(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgPong)
	})
}

func FuzzQueryChannelRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgQueryChannelRange)
	})
}

func FuzzZlibQueryShortChanIDs(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var buf bytes.Buffer
		zlibWriter := zlib.NewWriter(&buf)
		_, err := zlibWriter.Write(data)
		require.NoError(t, err) // Zlib bug?

		err = zlibWriter.Close()
		require.NoError(t, err) // Zlib bug?

		compressedPayload := buf.Bytes()

		chainhash := []byte("00000000000000000000000000000000")
		numBytesInBody := len(compressedPayload) + 1
		zlibByte := []byte("\x01")

		bodyBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(bodyBytes, uint16(numBytesInBody))

		payload := chainhash
		payload = append(payload, bodyBytes...)
		payload = append(payload, zlibByte...)
		payload = append(payload, compressedPayload...)

		wireMsgHarness(t, payload, MsgQueryShortChanIDs)
	})
}

func FuzzQueryShortChanIDs(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgQueryShortChanIDs)
	})
}

func FuzzZlibReplyChannelRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var buf bytes.Buffer
		zlibWriter := zlib.NewWriter(&buf)
		_, err := zlibWriter.Write(data)
		require.NoError(t, err) // Zlib bug?

		err = zlibWriter.Close()
		require.NoError(t, err) // Zlib bug?

		compressedPayload := buf.Bytes()

		// Initialize some []byte vars which will prefix our payload
		chainhash := []byte("00000000000000000000000000000000")
		firstBlockHeight := []byte("\x00\x00\x00\x00")
		numBlocks := []byte("\x00\x00\x00\x00")
		completeByte := []byte("\x00")

		numBytesInBody := len(compressedPayload) + 1
		zlibByte := []byte("\x01")

		bodyBytes := make([]byte, 2)
		binary.BigEndian.PutUint16(bodyBytes, uint16(numBytesInBody))

		payload := chainhash
		payload = append(payload, firstBlockHeight...)
		payload = append(payload, numBlocks...)
		payload = append(payload, completeByte...)
		payload = append(payload, bodyBytes...)
		payload = append(payload, zlibByte...)
		payload = append(payload, compressedPayload...)

		wireMsgHarness(t, payload, MsgReplyChannelRange)
	})
}

func FuzzReplyChannelRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// We can't use require.Equal for Timestamps, since we consider
		// the empty slice and nil to be equivalent.
		assertEq := func(t *testing.T, x, y any) {
			require.IsType(t, &ReplyChannelRange{}, x)
			first, _ := x.(*ReplyChannelRange)
			require.IsType(t, &ReplyChannelRange{}, y)
			second, _ := y.(*ReplyChannelRange)

			require.Equal(
				t, len(first.Timestamps),
				len(second.Timestamps),
			)
			for i, ts1 := range first.Timestamps {
				ts2 := second.Timestamps[i]
				require.Equal(t, ts1, ts2)
			}
			first.Timestamps = nil
			second.Timestamps = nil

			require.Equal(t, first, second)
		}

		wireMsgHarnessCustom(t, data, MsgReplyChannelRange, assertEq)
	})
}

func FuzzReplyShortChanIDsEnd(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgReplyShortChanIDsEnd)
	})
}

func FuzzRevokeAndAck(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgRevokeAndAck)
	})
}

func FuzzShutdown(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgShutdown)
	})
}

func FuzzUpdateAddHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgUpdateAddHTLC)
	})
}

func FuzzUpdateFailHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgUpdateFailHTLC)
	})
}

func FuzzUpdateFailMalformedHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgUpdateFailMalformedHTLC)
	})
}

func FuzzUpdateFee(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgUpdateFee)
	})
}

func FuzzUpdateFulfillHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgUpdateFulfillHTLC)
	})
}

func FuzzDynPropose(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgDynPropose)
	})
}

func FuzzDynReject(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgDynReject)
	})
}

func FuzzDynAck(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgDynAck)
	})
}

func FuzzDynCommit(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgDynCommit)
	})
}

func FuzzKickoffSig(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgKickoffSig)
	})
}

func FuzzCustomMessage(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, customMessageType uint16) {
		if customMessageType < uint16(CustomTypeStart) {
			customMessageType += uint16(CustomTypeStart)
		}

		wireMsgHarness(t, data, MessageType(customMessageType))
	})
}

func FuzzOnionMessage(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgOnionMessage)
	})
}

// FuzzParseRawSignature tests that our DER-encoded signature parsing does not
// panic for arbitrary inputs and that serializing and reparsing the signatures
// does not mutate them.
func FuzzParseRawSignature(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		sig, err := NewSigFromECDSARawSignature(data)
		if err != nil {
			return
		}

		sig2, err := NewSigFromECDSARawSignature(sig.ToSignatureBytes())
		require.NoError(t, err, "failed to reparse signature")

		require.Equal(t, sig, sig2, "signature mismatch")
	})
}

// FuzzConvertFixedSignature tests that conversion of fixed 64-byte signatures
// to DER-encoded signatures does not panic and that parsing and reconverting
// the signatures does not mutate them.
func FuzzConvertFixedSignature(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var sig Sig
		if len(data) > len(sig.bytes[:]) {
			return
		}
		copy(sig.bytes[:], data)

		derSig, err := sig.ToSignature()
		if err != nil {
			return
		}

		sig2, err := NewSigFromSignature(derSig)
		require.NoError(t, err, "failed to parse signature")

		derSig2, err := sig2.ToSignature()
		require.NoError(t, err, "failed to reconvert signature to DER")

		derBytes := derSig.Serialize()
		derBytes2 := derSig2.Serialize()
		require.Equal(t, derBytes, derBytes2, "signature mismatch")
	})
}

// FuzzConvertFixedSchnorrSignature tests that conversion of fixed 64-byte
// Schnorr signatures to and from the btcec format does not panic or mutate the
// signatures.
func FuzzConvertFixedSchnorrSignature(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		var sig Sig
		if len(data) > len(sig.bytes[:]) {
			return
		}
		copy(sig.bytes[:], data)
		sig.ForceSchnorr()

		btcecSig, err := sig.ToSignature()
		if err != nil {
			return
		}

		sig2, err := NewSigFromSignature(btcecSig)
		require.NoError(t, err, "failed to parse signature")

		btcecSig2, err := sig2.ToSignature()
		require.NoError(
			t, err, "failed to reconvert signature to btcec format",
		)

		btcecBytes := btcecSig.Serialize()
		btcecBytes2 := btcecSig2.Serialize()
		require.Equal(t, btcecBytes, btcecBytes2, "signature mismatch")
	})
}

// prefixWithFailCode adds a failure code prefix to data.
func prefixWithFailCode(data []byte, code FailCode) []byte {
	var codeBytes [2]byte
	binary.BigEndian.PutUint16(codeBytes[:], uint16(code))
	data = append(codeBytes[:], data...)

	return data
}

// onionFailureHarnessCustom performs the actual fuzz testing of the appropriate
// onion failure message. This function will check that the passed-in message
// passes wire length checks, is a valid message once deserialized, and passes a
// sequence of serialization and deserialization checks.
func onionFailureHarnessCustom(t *testing.T, data []byte, code FailCode,
	assertEqual assertEqualFunc) {

	data = prefixWithFailCode(data, code)

	// Don't waste time fuzzing messages larger than we'll ever accept.
	if len(data) > MaxSliceLength {
		return
	}

	// First check whether the failure message can be decoded.
	r := bytes.NewReader(data)
	msg, err := DecodeFailureMessage(r, 0)
	if err != nil {
		return
	}

	// We now have a valid decoded message. Verify that encoding and
	// decoding the message does not mutate it.

	var b bytes.Buffer
	err = EncodeFailureMessage(&b, msg, 0)
	require.NoError(t, err, "failed to encode failure message")

	newMsg, err := DecodeFailureMessage(&b, 0)
	require.NoError(t, err, "failed to decode serialized failure message")

	assertEqual(t, msg, newMsg)

	// Now verify that encoding/decoding full packets works as expected.

	var pktBuf bytes.Buffer
	if err := EncodeFailure(&pktBuf, msg, 0); err != nil {
		// EncodeFailure returns an error if the encoded message would
		// exceed FailureMessageLength bytes, as LND always encodes
		// fixed-size packets for privacy. But it is valid to decode
		// messages longer than this, so we should not report an error
		// if the original message was longer.
		//
		// We add 2 to the length of the original message since it may
		// have omitted a channel_update type prefix of 2 bytes. When
		// we re-encode such a message, we will add the 2-byte prefix
		// as prescribed by the spec.
		if len(data)+2 > FailureMessageLength {
			return
		}

		t.Fatalf("failed to encode failure packet: %v", err)
	}

	// We should use FailureMessageLength sized packets plus 2 bytes to
	// encode the message length and 2 bytes to encode the padding length,
	// as recommended by the spec.
	require.Equal(
		t, pktBuf.Len(), FailureMessageLength+4,
		"wrong failure message length",
	)

	pktMsg, err := DecodeFailure(&pktBuf, 0)
	require.NoError(t, err, "failed to decode failure packet")

	assertEqual(t, msg, pktMsg)
}

func onionFailureHarness(t *testing.T, data []byte, code FailCode) {
	t.Helper()
	assertEq := func(t *testing.T, x, y any) {
		require.Equal(t, x, y)
	}
	onionFailureHarnessCustom(t, data, code, assertEq)
}

func FuzzFailIncorrectDetails(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Since FailIncorrectDetails.Decode can leave extraOpaqueData
		// as nil while FailIncorrectDetails.Encode writes an empty
		// slice, we need to use a custom equality function.
		assertEq := func(t *testing.T, x, y any) {
			msg1, ok := x.(*FailIncorrectDetails)
			require.True(
				t, ok, "msg1 was not FailIncorrectDetails",
			)

			msg2, ok := y.(*FailIncorrectDetails)
			require.True(
				t, ok, "msg2 was not FailIncorrectDetails",
			)

			require.Equal(t, msg1.amount, msg2.amount)
			require.Equal(t, msg1.height, msg2.height)
			require.True(
				t, bytes.Equal(
					msg1.extraOpaqueData,
					msg2.extraOpaqueData,
				),
			)
		}

		onionFailureHarnessCustom(
			t, data, CodeIncorrectOrUnknownPaymentDetails, assertEq,
		)
	})
}

func FuzzFailInvalidOnionVersion(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeInvalidOnionVersion)
	})
}

func FuzzFailInvalidOnionHmac(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeInvalidOnionHmac)
	})
}

func FuzzFailInvalidOnionKey(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeInvalidOnionKey)
	})
}

func FuzzFailTemporaryChannelFailure(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeTemporaryChannelFailure)
	})
}

func FuzzFailAmountBelowMinimum(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeAmountBelowMinimum)
	})
}

func FuzzFailFeeInsufficient(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeFeeInsufficient)
	})
}

func FuzzFailIncorrectCltvExpiry(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeIncorrectCltvExpiry)
	})
}

func FuzzFailExpiryTooSoon(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeExpiryTooSoon)
	})
}

func FuzzFailChannelDisabled(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeChannelDisabled)
	})
}

func FuzzFailFinalIncorrectCltvExpiry(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeFinalIncorrectCltvExpiry)
	})
}

func FuzzFailFinalIncorrectHtlcAmount(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeFinalIncorrectHtlcAmount)
	})
}

func FuzzInvalidOnionPayload(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeInvalidOnionPayload)
	})
}

func FuzzFailInvalidBlinding(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		onionFailureHarness(t, data, CodeInvalidBlinding)
	})
}

func FuzzClosingSig(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgClosingSig)
	})
}

func FuzzClosingComplete(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		wireMsgHarness(t, data, MsgClosingComplete)
	})
}

// FuzzFee tests that decoding and re-encoding a Fee TLV does not mutate it.
func FuzzFee(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8 {
			return
		}

		var fee Fee
		var buf [8]byte
		r := bytes.NewReader(data)

		if err := feeDecoder(r, &fee, &buf, 8); err != nil {
			return
		}

		var b bytes.Buffer
		require.NoError(t, feeEncoder(&b, &fee, &buf))

		// Use bytes.Equal instead of require.Equal so that nil and
		// empty slices are considered equal.
		require.True(
			t, bytes.Equal(data, b.Bytes()), "%v != %v", data,
			b.Bytes(),
		)
	})
}
