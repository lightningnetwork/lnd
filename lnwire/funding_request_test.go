package lnwire

import (
	"bytes"
	"encoding/hex"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	//	"io"
	"io/ioutil"
	"reflect"
	"testing"
)

var (
	//For debugging, writes to /dev/shm/
	//Maybe in the future do it if you do "go test -v"
	WRITE_FILE = false
	FILENAME   = "/dev/shm/fundingRequest.raw"

	//preimage: 9a2cbd088763db88dd8ba79e5726daa6aba4aa7e
	//echo -n | openssl sha256 | openssl ripemd160 | openssl sha256 | openssl ripemd160
	revocationHashBytes, _ = hex.DecodeString("4132b6b48371f7b022a16eacb9b2b0ebee134d41")
	revocationHash         [20]byte
	_                      = copy(revocationHash[:], revocationHashBytes)

	//privkey: 9fa1d55217f57019a3c37f49465896b15836f54cb8ef6963870a52926420a2dd
	pubKeyBytes, _ = hex.DecodeString("02f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee")
	pubKey, _      = btcec.ParsePubKey(pubKeyBytes, btcec.S256())

	// Delivery PkScript
	//Privkey: f2c00ead9cbcfec63098dc0a5f152c0165aff40a2ab92feb4e24869a284c32a7
	//PKhash: n2fkWVphUzw3zSigzPsv9GuDyg9mohzKpz
	deliveryPkScript, _ = hex.DecodeString("76a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac")

	// Change PkScript
	//Privkey: 5b18f5049efd9d3aff1fb9a06506c0b809fb71562b6ecd02f6c5b3ab298f3b0f
	//PKhash: miky84cHvLuk6jcT6GsSbgHR8d7eZCu9Qc
	changePkScript, _ = hex.DecodeString("76a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac")

	//echo -n | openssl sha256
	//This stuff gets reversed!!!
	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	shaHash1, _      = wire.NewShaHash(shaHash1Bytes)
	outpoint1        = wire.NewOutPoint(shaHash1, 0)
	//echo | openssl sha256
	//This stuff gets reversed!!!
	shaHash2Bytes, _ = hex.DecodeString("01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b")
	shaHash2, _      = wire.NewShaHash(shaHash2Bytes)
	outpoint2        = wire.NewOutPoint(shaHash2, 1)
	//create inputs from outpoint1 and outpoint2
	inputs = []*wire.TxIn{wire.NewTxIn(outpoint1, nil), wire.NewTxIn(outpoint2, nil)}

	//funding request
	fundingRequest = &FundingRequest{
		ChannelType:           uint8(0),
		FundingAmount:         btcutil.Amount(100000000),
		ReserveAmount:         btcutil.Amount(131072),
		MinFeePerKb:           btcutil.Amount(20000),
		MinTotalFundingAmount: btcutil.Amount(150000000),
		LockTime:              uint32(4320), //30 block-days
		FeePayer:              uint8(0),
		RevocationHash:        revocationHash,
		Pubkey:                pubKey,
		DeliveryPkScript:      deliveryPkScript,
		ChangePkScript:        changePkScript,
		Inputs:                inputs,
	}
	serializedString  = "000000000005f5e1000000000008f0d1804132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e20000010e0001976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
	serializedMessage = "0709110b000000c8000000d8000000000005f5e1000000000008f0d1804132b6b48371f7b022a16eacb9b2b0ebee134d4102f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e20000010e0001976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac1976a914238ee44bb5c8c1314dd03974a17ec6c406fdcb8388ac02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
)

func TestFundingRequestEncodeDecode(t *testing.T) {
	//Test serialization
	b := new(bytes.Buffer)
	err := fundingRequest.Encode(b, 0)
	if err != nil {
		t.Error("Serialization error")
		t.Error(err.Error())
	} else {
		t.Logf("Encoded Funding Request: %x\n", b.Bytes())
		//Check if we serialized correctly
		if serializedString != hex.EncodeToString(b.Bytes()) {
			t.Error("Serialization does not match expected")
		}

		//So I can do: hexdump -C /dev/shm/fundingRequest.raw
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

	newFunding := NewFundingRequest()
	err = newFunding.Decode(c, 0)
	if err != nil {
		t.Error("Decoding Error")
		t.Error(err.Error())
	} else {
		if !reflect.DeepEqual(newFunding, fundingRequest) {
			t.Error("Decoding does not match!")
		}
		//Show the struct
		t.Log(newFunding.String())
	}

	//Test message using Message interface
	//Serialize/Encode
	b = new(bytes.Buffer)
	_, err = WriteMessage(b, fundingRequest, uint32(1), wire.TestNet3)
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
		if !reflect.DeepEqual(msg, fundingRequest) {
			t.Error("Message decoding does not match!")
		}
		t.Logf(msg.String())
	}
}
