package lnwire

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestKnownAcceptChannel2Message tests decoding and encoding of an
// accept_channel2 wire message created by CLN.
func TestKnownAcceptChannel2Message(t *testing.T) {
	t.Parallel()

	// Decode the known serialized message.
	knownEncodedMsg := hexToBytes(t, "00412aa51d05d2a4cc27183fcdc3f78cb878"+
		"12a617d8b843369e3c7bb51222898db200000000000000000000000000000"+
		"222ffffffffffffffff000000000000000000000001000501e302e3bd3800"+
		"9866c9da8ec4aa99cc4ea9c6c0dd46df15c61ef0ce1f271291714e5703cdc"+
		"b22e07f0f83805ae79d0fa1b777dc1dbd27c1dd2840469d72cf305332d663"+
		"02abc10666592840eb562f2afaedfac56930b4482ec5d8b61b5a4485b383c"+
		"2cba80331446fd843787dae9f2a68633aa17438c566f8a9a5f2115ab4dc2d"+
		"2753baa5b803e0a7bb422b254f54bc954be05bd6823a7b7a4b996ff8d3079"+
		"ca211590fb5df390371b7132cde15d9f729b6c24b08d808b59599806f7af5"+
		"e8be6811a0874c3c097700160014c2ccab171c2a5be9dab52ec41b8258630"+
		"24c546601021000")
	buf := bytes.NewBuffer(knownEncodedMsg)
	msg, err := ReadMessage(buf, 0)
	require.NoError(t, err, "failed to decode AcceptChannel2")
	decoded, ok := msg.(*AcceptChannel2)
	require.True(t, ok)

	// Verify the decoded message has the values we expect.
	expected := &AcceptChannel2{
		FundingAmount:    0,
		DustLimit:        546,
		MaxValueInFlight: 18446744073709551615,
		HtlcMinimum:      0,
		MinAcceptDepth:   1,
		CsvDelay:         5,
		MaxAcceptedHTLCs: 483,

		FundingKey: hexToPubKey(t, "02e3bd38009866c9da8ec4aa99cc4ea9c6"+
			"c0dd46df15c61ef0ce1f271291714e57"),

		RevocationPoint: hexToPubKey(t, "03cdcb22e07f0f83805ae79d0fa1b"+
			"777dc1dbd27c1dd2840469d72cf305332d663"),

		PaymentPoint: hexToPubKey(t, "02abc10666592840eb562f2afaedfac5"+
			"6930b4482ec5d8b61b5a4485b383c2cba8"),

		DelayedPaymentPoint: hexToPubKey(t, "0331446fd843787dae9f2a686"+
			"33aa17438c566f8a9a5f2115ab4dc2d2753baa5b8"),

		HtlcPoint: hexToPubKey(t, "03e0a7bb422b254f54bc954be05bd6823a7"+
			"b7a4b996ff8d3079ca211590fb5df39"),

		FirstCommitmentPoint: hexToPubKey(t, "0371b7132cde15d9f729b6c2"+
			"4b08d808b59599806f7af5e8be6811a0874c3c0977"),

		UpfrontShutdownScript: DeliveryAddress([]byte{
			0x00, 0x14, 0xc2, 0xcc, 0xab, 0x17, 0x1c, 0x2a, 0x5b,
			0xe9, 0xda, 0xb5, 0x2e, 0xc4, 0x1b, 0x82, 0x58, 0x63,
			0x02, 0x4c, 0x54, 0x66,
		}),
		ChannelType: (*ChannelType)(NewRawFeatureVector(12)),
		ExtraData: ExtraOpaqueData{
			0x00, 0x16, 0x00, 0x14, 0xc2, 0xcc, 0xab, 0x17, 0x1c,
			0x2a, 0x5b, 0xe9, 0xda, 0xb5, 0x2e, 0xc4, 0x1b, 0x82,
			0x58, 0x63, 0x02, 0x4c, 0x54, 0x66, 0x01, 0x02, 0x10,
			0x00,
		},
	}

	copy(expected.PendingChannelID[:], hexToBytes(t, "2aa51d05d2a4cc27183f"+
		"cdc3f78cb87812a617d8b843369e3c7bb51222898db2"))

	require.Equal(t, expected, decoded)

	// Re-encode the message and verify it matches the original.
	buf = &bytes.Buffer{}
	_, err = WriteMessage(buf, decoded, 0)
	require.NoError(t, err, "failed to re-encode AcceptChannel2")

	require.Equal(t, knownEncodedMsg, buf.Bytes())
}
