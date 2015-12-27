package lnwire

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	//	"io/ioutil"
	"testing"
)

var (
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
	shaHash1Bytes, _ = hex.DecodeString("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	shaHash1, _      = wire.NewShaHash(shaHash1Bytes)
	outpoint1        = wire.NewOutPoint(shaHash1, 0)
	//echo | openssl sha256
	shaHash2Bytes, _ = hex.DecodeString("01ba4719c80b6fe911b091a7c05124b64eeece964e09c058ef8f9805daca546b")
	shaHash2, _      = wire.NewShaHash(shaHash2Bytes)
	outpoint2        = wire.NewOutPoint(shaHash2, 0)
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
)

func TestFundingRequestSerializeDeserialize(t *testing.T) {
	b := new(bytes.Buffer)
	err := createChannel.SerializeFundingRequest(b)
	if err != nil {
		fmt.Println("ERR")
		fmt.Println(err)
		t.Error("Serialization error")
	}
	t.Logf("ASDF: %x\n", b.Bytes())
}
