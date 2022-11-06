package htlcswitch

import (
	"encoding/hex"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/require"
)

// TestLongFailureMessage tests that longer failure messages can be interpreted
// correctly.
func TestLongFailureMessage(t *testing.T) {
	// Use long 1024-byte test vector from BOLT 04.
	sessionKeyBytes, _ := hex.DecodeString("4141414141414141414141414141414141414141414141414141414141414141")

	pathKeys := []string{
		"02eec7245d6b7d2ccb30380bfbe2a3648cd7a942653f5aa340edcea1f283686619",
		"0324653eac434488002cc06bbfb7f10fe18991e35f9fe4302dbea6d2353dc0ab1c",
		"027f31ebc5462c1fdce1b737ecff52d37d75dea43ce11c74d25aa297165faa2007",
		"032c0b7cf95324a07d05398b240174dc0c2be444d96b159aa6c7f7b1e668680991",
		"02edabbd16b41c8371b92ef2f04c1185b4f03b6dcd52ba9b78d9d7c89c8f221145",
	}

	reason, _ := hex.DecodeString("fc2ae85ef3418df2bfd655ea209c9467ec4e1afe08a15ef42e9e9fcd452a47163b44c192896f45610741c85ed4074212537e0d118d472ff3a559ae244acd9d783c65977765c5d4e00b723d00f12475aafaafff7b31c1be5a589e6e25f8da2959107206dd42bbcb43438129ce6cce2b6b4ae63edc76b876136ca5ea6cd1c6a04ca86eca143d15e53ccdc9e23953e49dc2f87bb11e5238cd6536e57387225b8fff3bf5f3e686fd08458ffe0211b87d64770db9353500af9b122828a006da754cf979738b4374e146ea79dd93656170b89c98c5f2299d6e9c0410c826c721950c780486cd6d5b7130380d7eaff994a8503a8fef3270ce94889fe996da66ed121741987010f785494415ca991b2e8b39ef2df6bde98efd2aec7d251b2772485194c8368451ad49c2354f9d30d95367bde316fec6cbdddc7dc0d25e99d3075e13d3de0822669861dafcd29de74eac48b64411987285491f98d78584d0c2a163b7221ea796fb28671b2bb91e38ef5e18aaf32c6c02f2fb690358872a1ed28166172631a82c2568d23238017188ebbd48944a147f6cdb3690d5f88e51371cb70adf1fa02afe4ed8b581afc8bcc5104922843a55d52acde09bc9d2b71a663e178788280f3c3eae127d21b0b95777976b3eb17be40a702c244d0e5f833ff49dae6403ff44b131e66df8b88e33ab0a58e379f2c34bf5113c66b9ea8241fc7aa2b1fa53cf4ed3cdd91d407730c66fb039ef3a36d4050dde37d34e80bcfe02a48a6b14ae28227b1627b5ad07608a7763a531f2ffc96dff850e8c583461831b19feffc783bc1beab6301f647e9617d14c92c4b1d63f5147ccda56a35df8ca4806b8884c4aa3c3cc6a174fdc2232404822569c01aba686c1df5eecc059ba97e9688c8b16b70f0d24eacfdba15db1c71f72af1b2af85bd168f0b0800483f115eeccd9b02adf03bdd4a88eab03e43ce342877af2b61f9d3d85497cd1c6b96674f3d4f07f635bb26add1e36835e321d70263b1c04234e222124dad30ffb9f2a138e3ef453442df1af7e566890aedee568093aa922dd62db188aa8361c55503f8e2c2e6ba93de744b55c15260f15ec8e69bb01048ca1fa7bbbd26975bde80930a5b95054688a0ea73af0353cc84b997626a987cc06a517e18f91e02908829d4f4efc011b9867bd9bfe04c5f94e4b9261d30cc39982eb7b250f12aee2a4cce0484ff34eebba89bc6e35bd48d3968e4ca2d77527212017e202141900152f2fd8af0ac3aa456aae13276a13b9b9492a9a636e18244654b3245f07b20eb76b8e1cea8c55e5427f08a63a16b0a633af67c8e48ef8e53519041c9138176eb14b8782c6c2ee76146b8490b97978ee73cd0104e12f483be5a4af414404618e9f6633c55dda6f22252cb793d3d16fae4f0e1431434e7acc8fa2c009d4f6e345ade172313d558a4e61b4377e31b8ed4e28f7cd13a7fe3f72a409bc3bdabfe0ba47a6d861e21f64d2fac706dab18b")

	sphinxPath := make([]*btcec.PublicKey, len(pathKeys))
	for i, sKey := range pathKeys {
		bKey, err := hex.DecodeString(sKey)
		require.NoError(t, err)

		key, err := btcec.ParsePubKey(bKey)
		require.NoError(t, err)

		sphinxPath[i] = key
	}

	sessionKey, _ := btcec.PrivKeyFromBytes(sessionKeyBytes)

	circuit := &sphinx.Circuit{
		SessionKey:  sessionKey,
		PaymentPath: sphinxPath,
	}

	errorDecryptor := &SphinxErrorDecrypter{
		OnionErrorDecrypter: sphinx.NewOnionErrorDecrypter(circuit),
	}

	// Assert that the failure message can still be extracted.
	failure, err := errorDecryptor.DecryptError(reason)
	require.NoError(t, err)

	incorrectDetails, ok := failure.msg.(*lnwire.FailIncorrectDetails)
	require.True(t, ok)

	var value varBytesRecordProducer

	extraData := incorrectDetails.ExtraOpaqueData()
	typeMap, err := extraData.ExtractRecords(&value)
	require.NoError(t, err)
	require.Len(t, typeMap, 1)

	expectedValue := make([]byte, 300)
	for i := range expectedValue {
		expectedValue[i] = 128
	}

	require.Equal(t, expectedValue, value.data)
}

type varBytesRecordProducer struct {
	data []byte
}

func (v *varBytesRecordProducer) Record() tlv.Record {
	return tlv.MakePrimitiveRecord(34000, &v.data)
}
