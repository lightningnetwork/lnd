package lnwire

import (
	"bytes"
	"encoding/hex"
	"fmt"
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

	//echo -n | openssl sha256 | openssl ripemd160
	ourRevocationHashBytes, _ = hex.DecodeString("9a2cbd088763db88dd8ba79e5726daa6aba4aa7e")
	ourRevocationHash         [20]byte
	_                         = copy(ourRevocationHash[:], ourRevocationHashBytes)

	//privkey: 9fa1d55217f57019a3c37f49465896b15836f54cb8ef6963870a52926420a2dd
	ourPubKeyBytes, _ = hex.DecodeString("02f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee")
	ourPubKey, _      = btcec.ParsePubKey(ourPubKeyBytes, btcec.S256())

	// Delivery PkScript
	//Privkey: f2c00ead9cbcfec63098dc0a5f152c0165aff40a2ab92feb4e24869a284c32a7
	//PKhash: n2fkWVphUzw3zSigzPsv9GuDyg9mohzKpz
	ourDeliveryPkScript, _ = hex.DecodeString("76a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac")

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
	ourInputs = []*wire.TxIn{wire.NewTxIn(outpoint1, nil), wire.NewTxIn(outpoint2, nil)}

	//funding request
	createChannel = CreateChannel{
		ChannelType:           uint8(0),
		FundingAmount:         btcutil.Amount(100000000),
		ReserveAmount:         btcutil.Amount(131072),
		MinFeePerKb:           btcutil.Amount(20000),
		MinTotalFundingAmount: btcutil.Amount(150000000),
		LockTime:              uint32(4320), //30 block-days
		FeePayer:              uint8(0),
		RevocationHash:        ourRevocationHash,
		Pubkey:                ourPubKey,
		DeliveryPkScript:      ourDeliveryPkScript,
		Inputs:                ourInputs,
	}
	serializedString = "30000000000005f5e1000000000008f0d1809a2cbd088763db88dd8ba79e5726daa6aba4aa7e02f977808cb9577897582d7524b562691e180953dd0008eb44e09594c539d6daee00000000000200000000000000004e20000010e0001976a914e8048c0fb75bdecc91ebfb99c174f4ece29ffbd488ac02e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550000000001ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b00000001"
)

func TestFundingRequestSerializeDeserialize(t *testing.T) {
	b := new(bytes.Buffer)
	err := createChannel.SerializeFundingRequest(b)
	if err != nil {
		fmt.Println(err)
		t.Error("Serialization error")
	}
	t.Logf("Serialized Funding Request: %x\n", b.Bytes())

	//Check if we serialized correctly
	if serializedString != hex.EncodeToString(b.Bytes()) {
		t.Error("Serialization does not match expected")
	}

	//So I can do: hexdump -C /dev/shm/fundingRequest.raw
	if WRITE_FILE {
		err = ioutil.WriteFile(FILENAME, b.Bytes(), 0644)
		if err != nil {
			t.Error("File write error")
		}
	}

	//Test deserialization
	//Make a new buffer just to be clean
	c := new(bytes.Buffer)
	c.Write(b.Bytes())

	var newChannel CreateChannel
	err = newChannel.DeserializeFundingRequest(c)
	if err != nil {
		fmt.Println(err)
		t.Error("Deserialiation Error")
	}

	if !reflect.DeepEqual(newChannel, createChannel) {
		t.Error("Deserialization does not match!")
	}

	t.Log(newChannel.String())
}
