package lnwire

import (
	"bytes"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
	"reflect"
	"testing"
)

func TestChannelAnnoucementEncodeDecode(t *testing.T) {
	ca := &ChannelAnnouncement{
		FirstNodeSig:     someSig,
		SecondNodeSig:    someSig,
		ChannelID:        someChannelID,
		FirstBitcoinSig:  someSig,
		SecondBitcoinSig: someSig,
		FirstNodeID:      pubKey,
		SecondNodeID:     pubKey,
		FirstBitcoinKey:  pubKey,
		SecondBitcoinKey: pubKey,
	}

	// Next encode the CA message into an empty bytes buffer.
	var b bytes.Buffer
	if err := ca.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode ChannelAnnouncement: %v", err)
	}

	// Deserialize the encoded CA message into a new empty struct.
	ca2 := &ChannelAnnouncement{}
	if err := ca2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode ChannelAnnouncement: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(ca, ca2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			ca, ca2)
	}
}

func TestChannelAnnoucementValidation(t *testing.T) {
	getKeys := func(s string) (*btcec.PrivateKey, *btcec.PublicKey) {
		return btcec.PrivKeyFromBytes(btcec.S256(), []byte(s))
	}

	firstNodePrivKey, firstNodePubKey := getKeys("node-id-1")
	secondNodePrivKey, secondNodePubKey := getKeys("node-id-2")
	firstBitcoinPrivKey, firstBitcoinPubKey := getKeys("bitcoin-key-1")
	secondBitcoinPrivKey, secondBitcoinPubKey := getKeys("bitcoin-key-2")

	var hash []byte

	hash = wire.DoubleSha256(firstNodePubKey.SerializeCompressed())
	firstBitcoinSig, _ := firstBitcoinPrivKey.Sign(hash)

	hash = wire.DoubleSha256(secondNodePubKey.SerializeCompressed())
	secondBitcoinSig, _ := secondBitcoinPrivKey.Sign(hash)

	ca := &ChannelAnnouncement{
		ChannelID:        someChannelID,
		FirstBitcoinSig:  firstBitcoinSig,
		SecondBitcoinSig: secondBitcoinSig,
		FirstNodeID:      firstNodePubKey,
		SecondNodeID:     secondNodePubKey,
		FirstBitcoinKey:  firstBitcoinPubKey,
		SecondBitcoinKey: secondBitcoinPubKey,
	}

	dataToSign, _ := ca.DataToSign()
	hash = wire.DoubleSha256(dataToSign)

	firstNodeSign, _ := firstNodePrivKey.Sign(hash)
	ca.FirstNodeSig = firstNodeSign

	secondNodeSign, _ := secondNodePrivKey.Sign(hash)
	ca.SecondNodeSig = secondNodeSign

	if err := ca.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestChannelAnnoucementBadValidation(t *testing.T) {
	getKeys := func(s string) (*btcec.PrivateKey, *btcec.PublicKey) {
		return btcec.PrivKeyFromBytes(btcec.S256(), []byte(s))
	}

	firstNodePrivKey, firstNodePubKey := getKeys("node-id-1")
	secondNodePrivKey, secondNodePubKey := getKeys("node-id-2")
	firstBitcoinPrivKey, _ := getKeys("bitcoin-key-1")
	secondBitcoinPrivKey, _ := getKeys("bitcoin-key-2")

	var hash []byte

	hash = wire.DoubleSha256(firstNodePubKey.SerializeCompressed())
	firstBitcoinSig, _ := firstBitcoinPrivKey.Sign(hash)

	hash = wire.DoubleSha256(secondNodePubKey.SerializeCompressed())
	secondBitcoinSig, _ := secondBitcoinPrivKey.Sign(hash)

	ca := &ChannelAnnouncement{
		ChannelID:        someChannelID,
		FirstBitcoinSig:  firstBitcoinSig,
		SecondBitcoinSig: secondBitcoinSig,
		FirstNodeID:      pubKey, // wrong pubkey
		SecondNodeID:     pubKey, // wrong pubkey
		FirstBitcoinKey:  pubKey, // wrong pubkey
		SecondBitcoinKey: pubKey, // wrong pubkey
	}

	dataToSign, _ := ca.DataToSign()
	hash = wire.DoubleSha256(dataToSign)

	firstNodeSign, _ := firstNodePrivKey.Sign(hash)
	ca.FirstNodeSig = firstNodeSign

	secondNodeSign, _ := secondNodePrivKey.Sign(hash)
	ca.SecondNodeSig = secondNodeSign

	if err := ca.Validate(); err == nil {
		t.Fatal("error should be raised")
	}
}
