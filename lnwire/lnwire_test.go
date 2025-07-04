package lnwire

import (
	"bytes"
	crand "crypto/rand"
	"encoding/hex"
	"math"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

var (
	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb" +
		"92427ae41e4649b934ca495991b7852b855")

	shaHash1, _ = chainhash.NewHash(shaHash1Bytes)
	outpoint1   = wire.NewOutPoint(shaHash1, 0)

	testRBytes, _ = hex.DecodeString("8ce2bc69281ce27da07e6683571" +
		"319d18e949ddfa2965fb6caa1bf0314f882d7")
	testSBytes, _ = hex.DecodeString("299105481d63e0f4bc2a" +
		"88121167221b6700d72a0ead154c03be696a292d24ae")
	testRScalar          = new(btcec.ModNScalar)
	testSScalar          = new(btcec.ModNScalar)
	_                    = testRScalar.SetByteSlice(testRBytes)
	_                    = testSScalar.SetByteSlice(testSBytes)
	testSig              = ecdsa.NewSignature(testRScalar, testSScalar)
	testSchnorrSigStr, _ = hex.DecodeString("04E7F9037658A92AFEB4F2" +
		"5BAE5339E3DDCA81A353493827D26F16D92308E49E2A25E9220867" +
		"8A2DF86970DA91B03A8AF8815A8A60498B358DAF560B347AA557")
	testSchnorrSig, _ = NewSigFromSchnorrRawSignature(testSchnorrSigStr)
)

const letterBytes = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// generateRandomBytes returns a slice of n random bytes.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := crand.Read(b)
	if err != nil {
		return nil, err
	}

	return b, nil
}

func randRawKey(t *testing.T) [33]byte {
	var n [33]byte

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	copy(n[:], priv.PubKey().SerializeCompressed())

	return n
}

func randPubKey() (*btcec.PublicKey, error) {
	priv, err := btcec.NewPrivateKey()
	if err != nil {
		return nil, err
	}

	return priv.PubKey(), nil
}

// pubkeyFromHex parses a Bitcoin public key from a hex encoded string.
func pubkeyFromHex(keyHex string) (*btcec.PublicKey, error) {
	pubKeyBytes, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(pubKeyBytes)
}

// TestChanUpdateChanFlags ensures that converting the ChanUpdateChanFlags and
// ChanUpdateMsgFlags bitfields to a string behaves as expected.
func TestChanUpdateChanFlags(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		flags    uint8
		expected string
	}{
		{
			flags:    0,
			expected: "00000000",
		},
		{
			flags:    1,
			expected: "00000001",
		},
		{
			flags:    3,
			expected: "00000011",
		},
		{
			flags:    255,
			expected: "11111111",
		},
	}

	for _, test := range testCases {
		chanFlag := ChanUpdateChanFlags(test.flags)
		toStr := chanFlag.String()
		if toStr != test.expected {
			t.Fatalf("expected %v, got %v",
				test.expected, toStr)
		}

		msgFlag := ChanUpdateMsgFlags(test.flags)
		toStr = msgFlag.String()
		if toStr != test.expected {
			t.Fatalf("expected %v, got %v",
				test.expected, toStr)
		}
	}
}

// TestDecodeUnknownAddressType shows that an unknown address type is correctly
// decoded and encoded.
func TestDecodeUnknownAddressType(t *testing.T) {
	// Add a normal, clearnet address.
	tcpAddr := &net.TCPAddr{
		IP:   net.IP{127, 0, 0, 1},
		Port: 8080,
	}

	// Add an onion address.
	onionAddr := &tor.OnionAddr{
		OnionService: "abcdefghijklmnop.onion",
		Port:         9065,
	}

	// Now add an address with an unknown type.
	var newAddrType addressType = math.MaxUint8
	data := make([]byte, 0, 16)
	data = append(data, uint8(newAddrType))
	opaqueAddrs := &OpaqueAddrs{
		Payload: data,
	}

	buffer := bytes.NewBuffer(make([]byte, 0, MaxMsgBody))
	err := WriteNetAddrs(
		buffer, []net.Addr{tcpAddr, onionAddr, opaqueAddrs},
	)
	require.NoError(t, err)

	// Now we attempt to parse the bytes and assert that we get an error.
	var addrs []net.Addr
	err = ReadElement(buffer, &addrs)
	require.NoError(t, err)
	require.Len(t, addrs, 3)
	require.Equal(t, tcpAddr.String(), addrs[0].String())
	require.Equal(t, onionAddr.String(), addrs[1].String())
	require.Equal(t, hex.EncodeToString(data), addrs[2].String())
}

func TestMaxOutPointIndex(t *testing.T) {
	t.Parallel()

	op := wire.OutPoint{
		Index: math.MaxUint32,
	}

	var b bytes.Buffer
	if err := WriteOutPoint(&b, op); err == nil {
		t.Fatalf("write of outPoint should fail, index exceeds 16-bits")
	}
}

func TestEmptyMessageUnknownType(t *testing.T) {
	t.Parallel()

	fakeType := CustomTypeStart - 1
	if _, err := makeEmptyMessage(fakeType); err == nil {
		t.Fatalf("should not be able to make an empty message of an " +
			"unknown type")
	}
}

// TestLightningWireProtocol uses the rapid property-based testing framework to
// verify that all message types can be serialized and deserialized correctly.
func TestLightningWireProtocol(t *testing.T) {
	t.Parallel()

	for msgType := MessageType(0); msgType < MsgEnd; msgType++ {
		// If MakeEmptyMessage returns an error, then this isn't yet a
		// used message type.
		if _, err := MakeEmptyMessage(msgType); err != nil {
			continue
		}

		t.Run(msgType.String(), rapid.MakeCheck(func(t *rapid.T) {
			// Create an empty message of the given type.
			m, err := MakeEmptyMessage(msgType)

			// An error means this isn't a valid message type, so we
			// skip it.
			if err != nil {
				return
			}

			// The message must support the message type interface.
			testMsg, ok := m.(TestMessage)
			require.True(
				t, ok, "message %v doesn't support TestMessage",
				msgType,
			)

			// Use the RandTestMessage method to create a randomized
			// message.
			msg := testMsg.RandTestMessage(t)

			// Serialize the message to a buffer.
			var b bytes.Buffer
			writtenBytes, err := WriteMessage(&b, msg, 0)
			require.NoError(t, err, "unable to write msg")

			// Check that the serialized payload is below the max
			// payload size, accounting for the message type size.
			payloadLen := uint32(writtenBytes) - 2
			require.LessOrEqual(
				t, payloadLen, uint32(MaxMsgBody),
				"msg payload constraint violated: %v > %v",
				payloadLen, MaxMsgBody,
			)

			// Deserialize the message from the buffer.
			newMsg, err := ReadMessage(&b, 0)
			require.NoError(t, err, "unable to read msg")

			// Verify the deserialized message matches the original.
			require.Equal(
				t, msg, newMsg,
				"message mismatch for type %s", msgType,
			)
		}))
	}
}
