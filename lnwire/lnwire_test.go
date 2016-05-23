package lnwire

import (
	"bytes"
	"encoding/hex"
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

// Common variables and functions for the message tests

var (
	// For debugging, writes to /dev/shm/
	// Maybe in the future do it if you do "go test -v"
	WRITE_FILE = false
	filename   = "/dev/shm/serialized.raw"

	// preimage: 9a2cbd088763db88dd8ba79e5726daa6aba4aa7e
	// echo -n | openssl sha256 | openssl ripemd160 | openssl sha256 | openssl ripemd160
	revocationHashBytes, _ = hex.DecodeString("4132b6b48371f7b022a16eacb9b2b0ebee134d41")
	revocationHash         [20]byte

	// preimage: "hello world"
	redemptionHashBytes, _ = hex.DecodeString("5b315ebabb0d8c0d94281caa2dfee69a1a00436e")
	redemptionHash         [20]byte

	// preimage: "next hop"
	nextHopBytes, _ = hex.DecodeString("94a9ded5a30fc5944cb1e2cbcd980f30616a1440")
	nextHop         [20]byte

	privKeyBytes, _ = hex.DecodeString("9fa1d55217f57019a3c37f49465896b15836f54cb8ef6963870a52926420a2dd")
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), privKeyBytes)
	address         = pubKey

	//  Delivery PkScript
	// Privkey: f2c00ead9cbcfec63098dc0a5f152c0165aff40a2ab92feb4e24869a284c32a7
	// PKhash: n2fkWVphUzw3zSigzPsv9GuDyg9mohzKpz
	deliveryPkScript, _ = hex.DecodeString("76a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac")

	//  Change PkScript
	// Privkey: 5b18f5049efd9d3aff1fb9a06506c0b809fb71562b6ecd02f6c5b3ab298f3b0f
	// PKhash: miky84cHvLuk6jcT6GsSbgHR8d7eZCu9Qc
	changePkScript, _ = hex.DecodeString("76a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac")

	// echo -n | openssl sha256
	// This stuff gets reversed!!!
	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	shaHash1, _      = wire.NewShaHash(shaHash1Bytes)
	outpoint1        = wire.NewOutPoint(shaHash1, 0)
	// echo | openssl sha256
	// This stuff gets reversed!!!
	shaHash2Bytes, _ = hex.DecodeString("01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b")
	shaHash2, _      = wire.NewShaHash(shaHash2Bytes)
	outpoint2        = wire.NewOutPoint(shaHash2, 1)
	// create inputs from outpoint1 and outpoint2
	inputs = []*wire.TxIn{wire.NewTxIn(outpoint1, nil, nil), wire.NewTxIn(outpoint2, nil, nil)}

	// Commitment Signature
	tx           = wire.NewMsgTx()
	emptybytes   = new([]byte)
	sigStr, _    = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, privKey)
	commitSig, _ = btcec.ParseSignature(sigStr, btcec.S256())

	// Funding TX Sig 1
	sig1privKeyBytes, _ = hex.DecodeString("927f5827d75dd2addeb532c0fa5ac9277565f981dd6d0d037b422be5f60bdbef")
	sig1privKey, _      = btcec.PrivKeyFromBytes(btcec.S256(), sig1privKeyBytes)
	sigStr1, _          = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, sig1privKey)
	commitSig1, _       = btcec.ParseSignature(sigStr1, btcec.S256())
	// Funding TX Sig 2
	sig2privKeyBytes, _ = hex.DecodeString("8a4ad188f6f4000495b765cfb6ffa591133a73019c45428ddd28f53bab551847")
	sig2privKey, _      = btcec.PrivKeyFromBytes(btcec.S256(), sig2privKeyBytes)
	sigStr2, _          = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, sig2privKey)
	commitSig2, _       = btcec.ParseSignature(sigStr2, btcec.S256())
	// Slice of Funding TX Sigs
	ptrFundingTXSigs = append(*new([]*btcec.Signature), commitSig1, commitSig2)

	// TxID
	txid = new(wire.ShaHash)
	// Reversed when displayed
	txidBytes, _ = hex.DecodeString("fd95c6e5c9d5bcf9cfc7231b6a438e46c518c724d0b04b75cc8fddf84a254e3a")
	_            = copy(txid[:], txidBytes)
)

func SerializeTest(t *testing.T, message Message, expectedString string, filename string) *bytes.Buffer {
	var err error

	b := new(bytes.Buffer)
	err = message.Encode(b, 0)

	if err != nil {
		t.Errorf(err.Error())
	} else {
		t.Logf("Encoded Bytes: %x\n", b.Bytes())
		// Check if we serialized correctly
		if expectedString != hex.EncodeToString(b.Bytes()) {
			t.Error("Serialization does not match expected")
		}

		// So I can do: hexdump -C /dev/shm/fundingRequest.raw
		if WRITE_FILE {
			err = ioutil.WriteFile(filename, b.Bytes(), 0644)
			if err != nil {
				t.Error(err.Error())
			}
		}
	}
	return b
}

func DeserializeTest(t *testing.T, buf *bytes.Buffer, message Message, originalMessage Message) {
	var err error
	// Make a new buffer just to be clean
	c := new(bytes.Buffer)
	c.Write(buf.Bytes())

	err = message.Decode(c, 0)
	if err != nil {
		t.Error("Decoding Error")
		t.Error(err.Error())
	} else {
		if !reflect.DeepEqual(message, originalMessage) {
			t.Error("Decoding does not match!")
		}
		// Show the struct
		t.Log(message.String())
	}
}

func MessageSerializeDeserializeTest(t *testing.T, message Message, expectedString string) {
	var err error

	b := new(bytes.Buffer)
	_, err = WriteMessage(b, message, uint32(1), wire.TestNet3)
	t.Logf("%x\n", b.Bytes())
	if hex.EncodeToString(b.Bytes()) != expectedString {
		t.Error("Message encoding error")
	}
	// Deserialize/Decode
	c := new(bytes.Buffer)
	c.Write(b.Bytes())
	_, newMsg, _, err := ReadMessage(c, uint32(1), wire.TestNet3)
	if err != nil {
		t.Errorf(err.Error())
	} else {
		if !reflect.DeepEqual(newMsg, message) {
			t.Error("Message decoding does not match!")
		}
		t.Logf(newMsg.String())
	}
}
