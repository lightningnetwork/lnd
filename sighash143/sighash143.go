package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"strings"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/uspv"
)

const (
	inspk0 = "2103c9f4836b9a4f77fc0d81f7bcb01b7f1b35916864b9476c241ce9fc198bd25432ac"
	inamt0 = uint64(625000000)

	inspk1 = "00141d0f172a0ecb48aee1be1f2687d2963ae33f71a1"
	inamt1 = uint64(600000000)
)

func outpointBytesLil(op wire.OutPoint) []byte {
	var buf bytes.Buffer
	// ignore errors because.. whatever
	_ = binary.Write(&buf, binary.LittleEndian, op.Index)

	b := op.Hash[:]
	return append(b, buf.Bytes()...)
}

func calcSignatureHash(
	hashType txscript.SigHashType, tx *wire.MsgTx, idx int) []byte {

	return nil
}

// if sighash_ALL, hash of all txin outpoints, sequentially.
// if other sighash type, 0x00 * 32
func calcHashPrevOuts(tx *wire.MsgTx) [32]byte {

	for _, in := range tx.TxIn {
		//		in.PreviousOutPoint.Hash
	}

	return wire.DoubleSha256SH(tx.TxIn[0].SignatureScript)
}

func main() {
	fmt.Printf("sighash 143\n")

	// get previous pkscripts for inputs 0 and 1 from hex
	in0spk, err := hex.DecodeString(string(inspk0))
	if err != nil {
		log.Fatal(err)
	}
	in1spk, err := hex.DecodeString(string(inspk1))
	if err != nil {
		log.Fatal(err)
	}

	// load tx skeleton from local file
	fmt.Printf("loading tx from file tx.hex\n")
	txhex, err := ioutil.ReadFile("tx.hex")
	if err != nil {
		log.Fatal(err)
	}

	txhex = []byte(strings.TrimSpace(string(txhex)))
	txbytes, err := hex.DecodeString(string(txhex))
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("loaded %d byte tx %x\n", len(txbytes), txbytes)

	// 	make tx
	ttx := wire.NewMsgTx()

	// deserialize into tx
	buf := bytes.NewBuffer(txbytes)
	ttx.Deserialize(buf)

	ttx.TxIn[0].SignatureScript = in0spk
	ttx.TxIn[1].SignatureScript = in1spk

	fmt.Printf(uspv.TxToString(ttx))

}
