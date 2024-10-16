package netann

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCreateChanAnnouncement(t *testing.T) {
	t.Parallel()

	key := [33]byte{0x1}
	var sig lnwire.Sig
	features := lnwire.NewRawFeatureVector(lnwire.AnchorsRequired)
	expChanAnn := &lnwire.ChannelAnnouncement1{
		ChainHash:       chainhash.Hash{0x1},
		ShortChannelID:  lnwire.ShortChannelID{BlockHeight: 1},
		NodeID1:         key,
		NodeID2:         key,
		NodeSig1:        sig,
		NodeSig2:        sig,
		BitcoinKey1:     key,
		BitcoinKey2:     key,
		BitcoinSig1:     sig,
		BitcoinSig2:     sig,
		Features:        features,
		ExtraOpaqueData: []byte{0x1},
	}

	chanProof := &models.ChannelAuthProof{
		NodeSig1Bytes:    expChanAnn.NodeSig1.ToSignatureBytes(),
		NodeSig2Bytes:    expChanAnn.NodeSig2.ToSignatureBytes(),
		BitcoinSig1Bytes: expChanAnn.BitcoinSig1.ToSignatureBytes(),
		BitcoinSig2Bytes: expChanAnn.BitcoinSig2.ToSignatureBytes(),
	}
	chanInfo := &models.ChannelEdgeInfo{
		ChainHash:        expChanAnn.ChainHash,
		ChannelID:        expChanAnn.ShortChannelID.ToUint64(),
		ChannelPoint:     wire.OutPoint{Index: 1},
		Capacity:         btcutil.SatoshiPerBitcoin,
		NodeKey1Bytes:    key,
		NodeKey2Bytes:    key,
		BitcoinKey1Bytes: key,
		BitcoinKey2Bytes: key,
		Features: lnwire.NewFeatureVector(
			features, lnwire.Features,
		),
		ExtraOpaqueData: expChanAnn.ExtraOpaqueData,
	}
	chanAnn, _, _, err := CreateChanAnnouncement(
		chanProof, chanInfo, nil, nil,
	)
	require.NoError(t, err, "unable to create channel announcement")

	assert.Equal(t, chanAnn, expChanAnn)
}

// TestChanAnnounce2Validation checks that the various forms of the
// channel_announcement_2 message are validated correctly.
func TestChanAnnounce2Validation(t *testing.T) {
	t.Parallel()

	t.Run(
		"test 4-of-4 MuSig2 P2TR channel announcement",
		test4of4MuSig2P2TRChanAnnouncement,
	)

	t.Run(
		"test 3-of-3 MuSig2 P2TR channel announcement",
		test3of3MuSig2ChanAnnouncement,
	)

	t.Run(
		"test 4-of-4 MuSig2 P2WSH channel announcement",
		test4of4MuSig2P2WSHChanAnnouncement,
	)
}

// test4of4MuSig2P2TRChanAnnouncement covers the case where the funding
// transaction PK script is a P2WSH. In this case, the signature should be valid
// for the MuSig2 4-of-4 aggregation of the node keys and the bitcoin keys.
func test4of4MuSig2P2WSHChanAnnouncement(t *testing.T) {
	t.Parallel()

	// Generate the keys for node 1 and node2.
	node1, node2 := genChanAnnKeys(t)

	// Build the unsigned channel announcement.
	ann := buildUnsignedChanAnnouncement(node1, node2, true)

	// Serialise the bytes that need to be signed.
	msg, err := ChanAnn2DigestToSign(ann)
	require.NoError(t, err)

	var msgBytes [32]byte
	copy(msgBytes[:], msg.CloneBytes())

	// Generate the 4 nonces required for producing the signature.
	var (
		node1NodeNonce = genNonceForPubKey(t, node1.nodePub)
		node1BtcNonce  = genNonceForPubKey(t, node1.btcPub)
		node2NodeNonce = genNonceForPubKey(t, node2.nodePub)
		node2BtcNonce  = genNonceForPubKey(t, node2.btcPub)
	)

	nonceAgg, err := musig2.AggregateNonces([][66]byte{
		node1NodeNonce.PubNonce,
		node1BtcNonce.PubNonce,
		node2NodeNonce.PubNonce,
		node2BtcNonce.PubNonce,
	})
	require.NoError(t, err)

	pubKeys := []*btcec.PublicKey{
		node1.nodePub, node2.nodePub, node1.btcPub, node2.btcPub,
	}

	// Let Node1 sign the announcement message with its node key.
	psA1, err := musig2.Sign(
		node1NodeNonce.SecNonce, node1.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node1 sign the announcement message with its bitcoin key.
	psA2, err := musig2.Sign(
		node1BtcNonce.SecNonce, node1.btcPriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node2 sign the announcement message with its node key.
	psB1, err := musig2.Sign(
		node2NodeNonce.SecNonce, node2.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node2 sign the announcement message with its bitcoin key.
	psB2, err := musig2.Sign(
		node2BtcNonce.SecNonce, node2.btcPriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Finally, combine the partial signatures from Node1 and Node2 and add
	// the signature to the announcement message.
	s := musig2.CombineSigs(psA1.R, []*musig2.PartialSignature{
		psA1, psA2, psB1, psB2,
	})

	sig, err := lnwire.NewSigFromSignature(s)
	require.NoError(t, err)

	ann.Signature.Val = sig

	// Create an accurate representation of what the on-chain pk script will
	// look like. For this case, it is only important that we get the
	// correct script class.
	multiSigScript, err := input.GenMultiSigScript(
		node1.btcPub.SerializeCompressed(),
		node2.btcPub.SerializeCompressed(),
	)
	require.NoError(t, err)

	scriptHash, err := input.WitnessScriptHash(multiSigScript)
	require.NoError(t, err)
	pkAddr, err := btcutil.NewAddressScriptHash(
		scriptHash, &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	// Create a mock tx fetcher that returns the expected script class and
	// pk address.
	fetchTx := func(lnwire.ShortChannelID) (txscript.ScriptClass,
		btcutil.Address, error) {

		return txscript.WitnessV0ScriptHashTy, pkAddr, nil
	}

	// Validate the announcement.
	require.NoError(t, ValidateChannelAnn(ann, fetchTx))
}

// test4of4MuSig2P2TRChanAnnouncement covers the case where both bitcoin keys
// are present in the channel announcement 2 and the funding transaction PK
// script is a P2TR. In this case, the signature should be a 4-of-4 MuSig2.
func test4of4MuSig2P2TRChanAnnouncement(t *testing.T) {
	t.Parallel()

	// Generate the keys for node 1 and node2.
	node1, node2 := genChanAnnKeys(t)

	// Build the unsigned channel announcement.
	ann := buildUnsignedChanAnnouncement(node1, node2, true)

	// Serialise the bytes that need to be signed.
	msg, err := ChanAnn2DigestToSign(ann)
	require.NoError(t, err)

	var msgBytes [32]byte
	copy(msgBytes[:], msg.CloneBytes())

	// Generate the 4 nonces required for producing the signature.
	var (
		node1NodeNonce = genNonceForPubKey(t, node1.nodePub)
		node1BtcNonce  = genNonceForPubKey(t, node1.btcPub)
		node2NodeNonce = genNonceForPubKey(t, node2.nodePub)
		node2BtcNonce  = genNonceForPubKey(t, node2.btcPub)
	)

	nonceAgg, err := musig2.AggregateNonces([][66]byte{
		node1NodeNonce.PubNonce,
		node1BtcNonce.PubNonce,
		node2NodeNonce.PubNonce,
		node2BtcNonce.PubNonce,
	})
	require.NoError(t, err)

	pubKeys := []*btcec.PublicKey{
		node1.nodePub, node2.nodePub, node1.btcPub, node2.btcPub,
	}

	// Let Node1 sign the announcement message with its node key.
	psA1, err := musig2.Sign(
		node1NodeNonce.SecNonce, node1.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node1 sign the announcement message with its bitcoin key.
	psA2, err := musig2.Sign(
		node1BtcNonce.SecNonce, node1.btcPriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node2 sign the announcement message with its node key.
	psB1, err := musig2.Sign(
		node2NodeNonce.SecNonce, node2.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node2 sign the announcement message with its bitcoin key.
	psB2, err := musig2.Sign(
		node2BtcNonce.SecNonce, node2.btcPriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Finally, combine the partial signatures from Node1 and Node2 and add
	// the signature to the announcement message.
	s := musig2.CombineSigs(psA1.R, []*musig2.PartialSignature{
		psA1, psA2, psB1, psB2,
	})

	sig, err := lnwire.NewSigFromSignature(s)
	require.NoError(t, err)

	ann.Signature.Val = sig

	// Create an accurate representation of what the on-chain pk script will
	// look like. For this case, it is only important that we get the
	// correct script class.
	combinedKey, _, _, err := musig2.AggregateKeys(
		[]*btcec.PublicKey{node1.btcPub, node2.btcPub}, true,
	)
	require.NoError(t, err)

	pkAddr, err := btcutil.NewAddressTaproot(
		combinedKey.FinalKey.SerializeCompressed()[1:],
		&chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	// Create a mock tx fetcher that returns the expected script class and
	// pk address.
	fetchTx := func(lnwire.ShortChannelID) (txscript.ScriptClass,
		btcutil.Address, error) {

		return txscript.WitnessV1TaprootTy, pkAddr, nil
	}

	// Validate the announcement.
	require.NoError(t, ValidateChannelAnn(ann, fetchTx))
}

// test3of3MuSig2ChanAnnouncement covers the case where no bitcoin keys are
// present in the channel announcement. In this case, the signature should be
// a 3-of-3 MuSig2 where the keys making up the pub key are: node1 ID, node2 ID
// and the output key found on-chain in the funding transaction. As the
// verifier, we don't care about the construction of the output key. We only
// care that the channel peers were able to sign for the output key. In reality,
// this key will likely be constructed from at least 1 key from each peer and
// the partial signature for it will be constructed via nested MuSig2 but for
// the sake of the test, we will just have it be backed by a single key.
func test3of3MuSig2ChanAnnouncement(t *testing.T) {
	// Generate the keys for node 1 and node 2.
	node1, node2 := genChanAnnKeys(t)

	// Build the unsigned channel announcement.
	ann := buildUnsignedChanAnnouncement(node1, node2, false)

	// Serialise the bytes that need to be signed.
	msg, err := ChanAnn2DigestToSign(ann)
	require.NoError(t, err)

	var msgBytes [32]byte
	copy(msgBytes[:], msg.CloneBytes())

	// Create a random 3rd key to be used for the output key.
	outputKeyPriv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	outputKey := outputKeyPriv.PubKey()

	// Ensure that the output key has an even Y by negating the private key
	// if required.
	if outputKey.SerializeCompressed()[0] ==
		input.PubKeyFormatCompressedOdd {

		outputKeyPriv.Key.Negate()
		outputKey = outputKeyPriv.PubKey()
	}

	// Generate the nonces required for producing the partial signatures.
	var (
		node1NodeNonce = genNonceForPubKey(t, node1.nodePub)
		node2NodeNonce = genNonceForPubKey(t, node2.nodePub)
		outputKeyNonce = genNonceForPubKey(t, outputKey)
	)

	nonceAgg, err := musig2.AggregateNonces([][66]byte{
		node1NodeNonce.PubNonce,
		node2NodeNonce.PubNonce,
		outputKeyNonce.PubNonce,
	})
	require.NoError(t, err)

	pkAddr, err := btcutil.NewAddressTaproot(
		outputKey.SerializeCompressed()[1:], &chaincfg.MainNetParams,
	)
	require.NoError(t, err)

	// Create a mock tx fetcher that returns the expected script class
	// and pk address.
	fetchTx := func(lnwire.ShortChannelID) (txscript.ScriptClass,
		btcutil.Address, error) {

		return txscript.WitnessV1TaprootTy, pkAddr, nil
	}

	pubKeys := []*btcec.PublicKey{node1.nodePub, node2.nodePub, outputKey}

	// Let Node1 sign the announcement message with its node key.
	psA, err := musig2.Sign(
		node1NodeNonce.SecNonce, node1.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Let Node2 sign the announcement message with its node key.
	psB, err := musig2.Sign(
		node2NodeNonce.SecNonce, node2.nodePriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Create a partial sig for the output key.
	psO, err := musig2.Sign(
		outputKeyNonce.SecNonce, outputKeyPriv, nonceAgg, pubKeys,
		msgBytes, musig2.WithSortedKeys(),
	)
	require.NoError(t, err)

	// Finally, combine the partial signatures from Node1 and Node2 and add
	// the signature to the announcement message.
	s := musig2.CombineSigs(psA.R, []*musig2.PartialSignature{
		psA, psB, psO,
	})

	sig, err := lnwire.NewSigFromSignature(s)
	require.NoError(t, err)

	ann.Signature.Val = sig

	// Validate the announcement.
	require.NoError(t, ValidateChannelAnn(ann, fetchTx))
}

func genNonceForPubKey(t *testing.T, pub *btcec.PublicKey) *musig2.Nonces {
	nonce, err := musig2.GenNonces(musig2.WithPublicKey(pub))
	require.NoError(t, err)

	return nonce
}

type keyRing struct {
	nodePriv *btcec.PrivateKey
	nodePub  *btcec.PublicKey
	btcPriv  *btcec.PrivateKey
	btcPub   *btcec.PublicKey
}

func genChanAnnKeys(t *testing.T) (*keyRing, *keyRing) {
	// Let Alice and Bob derive the various keys they need.
	aliceNodePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	aliceNodeID := aliceNodePrivKey.PubKey()

	aliceBtcPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	bobNodePrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	bobNodeID := bobNodePrivKey.PubKey()

	bobBtcPrivKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	alice := &keyRing{
		nodePriv: aliceNodePrivKey,
		nodePub:  aliceNodePrivKey.PubKey(),
		btcPriv:  aliceBtcPrivKey,
		btcPub:   aliceBtcPrivKey.PubKey(),
	}

	bob := &keyRing{
		nodePriv: bobNodePrivKey,
		nodePub:  bobNodePrivKey.PubKey(),
		btcPriv:  bobBtcPrivKey,
		btcPub:   bobBtcPrivKey.PubKey(),
	}

	if bytes.Compare(
		aliceNodeID.SerializeCompressed(),
		bobNodeID.SerializeCompressed(),
	) != -1 {

		return bob, alice
	}

	return alice, bob
}

func buildUnsignedChanAnnouncement(node1, node2 *keyRing,
	withBtcKeys bool) *lnwire.ChannelAnnouncement2 {

	var ann lnwire.ChannelAnnouncement2
	ann.ChainHash.Val = *chaincfg.MainNetParams.GenesisHash
	features := lnwire.NewRawFeatureVector()
	ann.Features.Val = *features
	ann.ShortChannelID.Val = lnwire.ShortChannelID{
		BlockHeight: 1000,
		TxIndex:     100,
		TxPosition:  0,
	}
	ann.Capacity.Val = 100000

	copy(ann.NodeID1.Val[:], node1.nodePub.SerializeCompressed())
	copy(ann.NodeID2.Val[:], node2.nodePub.SerializeCompressed())

	if !withBtcKeys {
		return &ann
	}

	btcKey1Bytes := tlv.ZeroRecordT[tlv.TlvType12, [33]byte]()
	btcKey2Bytes := tlv.ZeroRecordT[tlv.TlvType14, [33]byte]()

	copy(btcKey1Bytes.Val[:], node1.btcPub.SerializeCompressed())
	copy(btcKey2Bytes.Val[:], node2.btcPub.SerializeCompressed())

	ann.BitcoinKey1 = tlv.SomeRecordT(btcKey1Bytes)
	ann.BitcoinKey2 = tlv.SomeRecordT(btcKey2Bytes)

	return &ann
}
