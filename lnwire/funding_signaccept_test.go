package lnwire

import (
	"bytes"
	"encoding/hex"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	//	"io"
	"io/ioutil"
	"reflect"
	"testing"
)

func TestFundingSignAcceptEncodeDecode(t *testing.T) {
	var (
		//For debugging, writes to /dev/shm/
		//Maybe in the future do it if you do "go test -v"
		WRITE_FILE = false
		FILENAME   = "/dev/shm/fundingSignAccept.raw"

		privKeyBytes, _ = hex.DecodeString("9fa1d55217f57019a3c37f49465896b15836f54cb8ef6963870a52926420a2dd")
		privKey, _      = btcec.PrivKeyFromBytes(btcec.S256(), privKeyBytes)
		//pubKeyBytes, _ = hex.DecodeString("02f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee")
		//pubKey, _      = btcec.ParsePubKey(pubKeyBytes, btcec.S256())

		//Commitment Signature
		tx           = wire.NewMsgTx()
		emptybytes   = new([]byte)
		sigStr, _    = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, privKey)
		commitSig, _ = btcec.ParseSignature(sigStr, btcec.S256())

		//Funding TX Sig 1
		sig1privKeyBytes, _ = hex.DecodeString("927f5827d75dd2addeb532c0fa5ac9277565f981dd6d0d037b422be5f60bdbef")
		sig1privKey, _      = btcec.PrivKeyFromBytes(btcec.S256(), sig1privKeyBytes)
		sigStr1, _          = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, sig1privKey)
		commitSig1, _       = btcec.ParseSignature(sigStr1, btcec.S256())
		//Funding TX Sig 2
		sig2privKeyBytes, _ = hex.DecodeString("8a4ad188f6f4000495b765cfb6ffa591133a73019c45428ddd28f53bab551847")
		sig2privKey, _      = btcec.PrivKeyFromBytes(btcec.S256(), sig2privKeyBytes)
		sigStr2, _          = txscript.RawTxInSignature(tx, 0, *emptybytes, txscript.SigHashAll, sig2privKey)
		commitSig2, _       = btcec.ParseSignature(sigStr2, btcec.S256())
		fundingTXSigs       = append(*new([]btcec.Signature), *commitSig1, *commitSig2)

		//funding response
		fundingSignAccept = &FundingSignAccept{
			ReservationID: uint64(12345678),
			CommitSig:     commitSig,
			FundingTXSigs: &fundingTXSigs,
		}
		serializedString  = "0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
		serializedMessage = "0709110b000000dc000000df0000000000bc614e4630440220333835e58e958f5e92b4ff4e6fa2470dac88094c97506b4d6d1f4e23e52cb481022057483ac18d6b9c9c14f0c626694c9ccf8b27b3dbbedfdf6b6c9a9fa9f427a1df02473045022100e7946d057c0b4cc4d3ea525ba156b429796858ebc543d75a6c6c2cbca732db6902202fea377c1f9fb98cd103cf5a4fba276a074b378d4227d15f5fa6439f1a6685bb4630440220235ee55fed634080089953048c3e3f7dc3a154fd7ad18f31dc08e05b7864608a02203bdd7d4e4d9a8162d4b511faf161f0bb16c45181187125017cd0c620c53876ca"
	)
	//Test serialization
	b := new(bytes.Buffer)
	err := fundingSignAccept.Encode(b, 0)
	if err != nil {
		t.Error("Serialization error")
		t.Error(err.Error())
	} else {
		t.Logf("Encoded Funding SignAccept: %x\n", b.Bytes())
		//Check if we serialized correctly
		if serializedString != hex.EncodeToString(b.Bytes()) {
			t.Error("Serialization does not match expected")
		}

		//So I can do: hexdump -C /dev/shm/fundingSignAccept.raw
		if WRITE_FILE {
			err = ioutil.WriteFile(FILENAME, b.Bytes(), 0644)
			if err != nil {
				t.Error("File write error")
				t.Error(err.Error())
			}
		}
	}

	//Test deserialization
	//Make a new buffer just to be clean
	c := new(bytes.Buffer)
	c.Write(b.Bytes())

	newFunding := NewFundingSignAccept()
	err = newFunding.Decode(c, 0)
	if err != nil {
		t.Error("Decoding Error")
		t.Error(err.Error())
	} else {
		if !reflect.DeepEqual(newFunding, fundingSignAccept) {
			t.Error("Decoding does not match!")
		}
		//Show the struct
		t.Log(newFunding.String())
	}

	//Test message using Message interface
	//Serialize/Encode
	b = new(bytes.Buffer)
	_, err = WriteMessage(b, fundingSignAccept, uint32(1), wire.TestNet3)
	t.Logf("%x\n", b.Bytes())
	if hex.EncodeToString(b.Bytes()) != serializedMessage {
		t.Error("Message encoding error")
	}
	//Deserialize/Decode
	c = new(bytes.Buffer)
	c.Write(b.Bytes())
	_, msg, _, err := ReadMessage(c, uint32(1), wire.TestNet3)
	if err != nil {
		t.Errorf(err.Error())
	} else {
		if !reflect.DeepEqual(msg, fundingSignAccept) {
			t.Error("Message decoding does not match!")
		}
		t.Logf(msg.String())
	}
}
