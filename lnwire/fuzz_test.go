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

// harness performs the actual fuzz testing of the appropriate wire message.
// This function will check that the passed-in message passes wire length
// checks, is a valid message once deserialized, and passes a sequence of
// serialization and deserialization checks.
func harness(t *testing.T, data []byte) {
	t.Helper()

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
	require.Equal(t, msg, newMsg)
}

func FuzzAcceptChannel(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		data = prefixWithMsgType(data, MsgAcceptChannel)
		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Make sure byte array length (excluding 2 bytes for message
		// type) is less than max payload size for the wire message.
		payloadLen := uint32(len(data)) - 2
		if payloadLen > MaxMsgBody {
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

		// Deserialize the message from the serialized bytes buffer, and
		// then assert that the original message is equal to the newly
		// deserialized message.
		newMsg, err := ReadMessage(&b, 0)
		require.NoError(t, err)

		require.IsType(t, &AcceptChannel{}, msg)
		first, _ := msg.(*AcceptChannel)
		require.IsType(t, &AcceptChannel{}, newMsg)
		second, _ := newMsg.(*AcceptChannel)

		// We can't use require.Equal for UpfrontShutdownScript, since
		// we consider the empty slice and nil to be equivalent.
		require.True(
			t, bytes.Equal(
				first.UpfrontShutdownScript,
				second.UpfrontShutdownScript,
			),
		)
		first.UpfrontShutdownScript = nil
		second.UpfrontShutdownScript = nil

		require.Equal(t, first, second)
	})
}

func FuzzAnnounceSignatures(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgAnnounceSignatures.
		data = prefixWithMsgType(data, MsgAnnounceSignatures)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzChannelAnnouncement(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgChannelAnnouncement.
		data = prefixWithMsgType(data, MsgChannelAnnouncement)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzChannelReestablish(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgChannelReestablish.
		data = prefixWithMsgType(data, MsgChannelReestablish)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzChannelUpdate(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgChannelUpdate.
		data = prefixWithMsgType(data, MsgChannelUpdate)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzClosingSigned(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgClosingSigned.
		data = prefixWithMsgType(data, MsgClosingSigned)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzCommitSig(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgCommitSig.
		data = prefixWithMsgType(data, MsgCommitSig)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzError(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgError.
		data = prefixWithMsgType(data, MsgError)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzWarning(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgWarning.
		data = prefixWithMsgType(data, MsgWarning)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzFundingCreated(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgFundingCreated.
		data = prefixWithMsgType(data, MsgFundingCreated)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzChannelReady(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgChannelReady.
		data = prefixWithMsgType(data, MsgChannelReady)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzFundingSigned(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgFundingSigned.
		data = prefixWithMsgType(data, MsgFundingSigned)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzGossipTimestampRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgGossipTimestampRange.
		data = prefixWithMsgType(data, MsgGossipTimestampRange)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzInit(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgInit.
		data = prefixWithMsgType(data, MsgInit)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzNodeAnnouncement(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgNodeAnnouncement.
		data = prefixWithMsgType(data, MsgNodeAnnouncement)

		// We have to do this here instead of in harness so that
		// reflect.DeepEqual isn't called. Address (de)serialization
		// messes up the fuzzing assertions.

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Make sure byte array length (excluding 2 bytes for message
		// type) is less than max payload size for the wire message.
		payloadLen := uint32(len(data)) - 2
		if payloadLen > MaxMsgBody {
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

		// Deserialize the message from the serialized bytes buffer, and
		// then assert that the original message is equal to the newly
		// deserialized message.
		newMsg, err := ReadMessage(&b, 0)
		require.NoError(t, err)

		require.IsType(t, &NodeAnnouncement{}, msg)
		first, _ := msg.(*NodeAnnouncement)
		require.IsType(t, &NodeAnnouncement{}, newMsg)
		second, _ := newMsg.(*NodeAnnouncement)

		// We can't use require.Equal for Addresses, since the same IP
		// can be represented by different underlying bytes. Instead, we
		// compare the normalized string representation of each address.
		require.Equal(t, len(first.Addresses), len(second.Addresses))
		for i := range first.Addresses {
			require.Equal(
				t, first.Addresses[i].String(),
				second.Addresses[i].String(),
			)
		}
		first.Addresses = nil
		second.Addresses = nil

		require.Equal(t, first, second)
	})
}

func FuzzOpenChannel(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgOpenChannel.
		data = prefixWithMsgType(data, MsgOpenChannel)

		// We have to do this here instead of in harness so that
		// reflect.DeepEqual isn't called. Because of the
		// UpfrontShutdownScript encoding, the first message and second
		// message aren't deeply equal since the first has a nil slice
		// and the other has an empty slice.

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Make sure byte array length (excluding 2 bytes for message
		// type) is less than max payload size for the wire message.
		payloadLen := uint32(len(data)) - 2
		if payloadLen > MaxMsgBody {
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

		// Deserialize the message from the serialized bytes buffer, and
		// then assert that the original message is equal to the newly
		// deserialized message.
		newMsg, err := ReadMessage(&b, 0)
		require.NoError(t, err)

		require.IsType(t, &OpenChannel{}, msg)
		first, _ := msg.(*OpenChannel)
		require.IsType(t, &OpenChannel{}, newMsg)
		second, _ := newMsg.(*OpenChannel)

		// We can't use require.Equal for UpfrontShutdownScript, since
		// we consider the empty slice and nil to be equivalent.
		require.True(
			t, bytes.Equal(
				first.UpfrontShutdownScript,
				second.UpfrontShutdownScript,
			),
		)
		first.UpfrontShutdownScript = nil
		second.UpfrontShutdownScript = nil

		require.Equal(t, first, second)
	})
}

func FuzzPing(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgPing.
		data = prefixWithMsgType(data, MsgPing)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzPong(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgPong.
		data = prefixWithMsgType(data, MsgPong)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzQueryChannelRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgQueryChannelRange.
		data = prefixWithMsgType(data, MsgQueryChannelRange)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
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

		// Prefix with MsgQueryShortChanIDs.
		payload = prefixWithMsgType(payload, MsgQueryShortChanIDs)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, payload)
	})
}

func FuzzQueryShortChanIDs(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgQueryShortChanIDs.
		data = prefixWithMsgType(data, MsgQueryShortChanIDs)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
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

		// Prefix with MsgReplyChannelRange.
		payload = prefixWithMsgType(payload, MsgReplyChannelRange)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, payload)
	})
}

func FuzzReplyChannelRange(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgReplyChannelRange.
		data = prefixWithMsgType(data, MsgReplyChannelRange)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzReplyShortChanIDsEnd(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgReplyShortChanIDsEnd.
		data = prefixWithMsgType(data, MsgReplyShortChanIDsEnd)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzRevokeAndAck(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgRevokeAndAck.
		data = prefixWithMsgType(data, MsgRevokeAndAck)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzShutdown(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgShutdown.
		data = prefixWithMsgType(data, MsgShutdown)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzUpdateAddHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgUpdateAddHTLC.
		data = prefixWithMsgType(data, MsgUpdateAddHTLC)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzUpdateFailHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgUpdateFailHTLC.
		data = prefixWithMsgType(data, MsgUpdateFailHTLC)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzUpdateFailMalformedHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgUpdateFailMalformedHTLC.
		data = prefixWithMsgType(data, MsgUpdateFailMalformedHTLC)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzUpdateFee(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgUpdateFee.
		data = prefixWithMsgType(data, MsgUpdateFee)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzUpdateFulfillHTLC(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Prefix with MsgUpdateFulFillHTLC.
		data = prefixWithMsgType(data, MsgUpdateFulfillHTLC)

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
	})
}

func FuzzCustomMessage(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte, customMessageType uint16) {
		if customMessageType < uint16(CustomTypeStart) {
			customMessageType += uint16(CustomTypeStart)
		}

		// Prefix with CustomMessage.
		data = prefixWithMsgType(data, MessageType(customMessageType))

		// Pass the message into our general fuzz harness for wire
		// messages!
		harness(t, data)
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
