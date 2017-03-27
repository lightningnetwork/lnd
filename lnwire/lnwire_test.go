package lnwire

import (
	"encoding/hex"
	"net"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
)

// Common variables and functions for the message tests
var (
	revHash = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	shaHash1, _      = chainhash.NewHash(shaHash1Bytes)
	outpoint1        = wire.NewOutPoint(shaHash1, 0)

	privKeyBytes, _ = hex.DecodeString("9fa1d55217f57019a3c37f49465896b15836f54cb8ef6963870a52926420a2dd")
	privKey, pubKey = btcec.PrivKeyFromBytes(btcec.S256(), privKeyBytes)
	address         = pubKey

	// Commitment Signature
	tx           = wire.NewMsgTx(1)
	emptybytes   = new([]byte)
	sigStr, _    = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, privKey)
	commitSig, _ = btcec.ParseSignature(sigStr, btcec.S256())

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
	txid = new(chainhash.Hash)
	// Reversed when displayed
	txidBytes, _ = hex.DecodeString("fd95c6e5c9d5bcf9cfc7231b6a438e46c518c724d0b04b75cc8fddf84a254e3a")
	_            = copy(txid[:], txidBytes)

	someAlias, _ = NewAlias("012345678901234567890")
	someSig, _   = btcec.ParseSignature(sigStr, btcec.S256())
	someSigBytes = someSig.Serialize()

	someAddress         = &net.TCPAddr{IP: (net.IP)([]byte{0x7f, 0x0, 0x0, 0x1}), Port: 8333}
	someOtherAddress, _ = net.ResolveTCPAddr("tcp", "[2001:db8:85a3:0:0:8a2e:370:7334]:80")
	someAddresses       = []net.Addr{someAddress, someOtherAddress}

	maxUint32 uint32 = (1 << 32) - 1
	maxUint24 uint32 = (1 << 24) - 1
	maxUint16 uint16 = (1 << 16) - 1

	someChannelID = ShortChannelID{
		BlockHeight: maxUint24,
		TxIndex:     maxUint24,
		TxPosition:  maxUint16,
	}

	someRGB = RGB{
		red:   255,
		green: 255,
		blue:  255,
	}

	someFeature  = featureName("somefeature")
	someFeatures = NewFeatureVector([]Feature{
		{someFeature, OptionalFlag},
	})
)
