package lnwire

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/wire"
)

func TestNodeAnnouncementEncodeDecode(t *testing.T) {
	cua := &NodeAnnouncement{
		Signature: someSig,
		Timestamp: maxUint32,
		Address:   someAddress,
		NodeID:    pubKey,
		RGBColor:  someRGB,
		pad:       maxUint16,
		Alias:     someAlias,
	}

	// Next encode the NA message into an empty bytes buffer.
	var b bytes.Buffer
	if err := cua.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode NodeAnnouncement: %v", err)
	}

	// Deserialize the encoded NA message into a new empty struct.
	cua2 := &NodeAnnouncement{}
	if err := cua2.Decode(&b, 0); err != nil {
		t.Fatalf("unable to decode NodeAnnouncement: %v", err)
	}

	// Assert equality of the two instances.
	if !reflect.DeepEqual(cua, cua2) {
		t.Fatalf("encode/decode error messages don't match %#v vs %#v",
			cua, cua2)
	}
}

func TestNodeAnnoucementValidation(t *testing.T) {
	getKeys := func(s string) (*btcec.PrivateKey, *btcec.PublicKey) {
		return btcec.PrivKeyFromBytes(btcec.S256(), []byte(s))
	}

	nodePrivKey, nodePubKey := getKeys("node-id-1")

	var hash []byte

	na := &NodeAnnouncement{
		Timestamp: maxUint32,
		Address:   someAddress,
		NodeID:    nodePubKey,
		RGBColor:  someRGB,
		pad:       maxUint16,
		Alias:     someAlias,
	}

	dataToSign, _ := na.DataToSign()
	hash = wire.DoubleSha256(dataToSign)

	signature, _ := nodePrivKey.Sign(hash)
	na.Signature = signature

	if err := na.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeAnnoucementPayloadLength(t *testing.T) {
	na := &NodeAnnouncement{
		Signature: someSig,
		Timestamp: maxUint32,
		Address:   someAddress,
		NodeID:    pubKey,
		RGBColor:  someRGB,
		pad:       maxUint16,
		Alias:     someAlias,
	}

	var b bytes.Buffer
	if err := na.Encode(&b, 0); err != nil {
		t.Fatalf("unable to encode node: %v", err)
	}

	serializedLength := uint32(b.Len())
	if serializedLength != na.MaxPayloadLength(0) {
		t.Fatalf("payload length estimate is incorrect: expected %v "+
			"got %v", serializedLength, na.MaxPayloadLength(0))
	}
}
