package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcutil"

	"github.com/lightningnetwork/lnd/uspv"
)

/* this is a CLI shell for testing out LND.  Right now it's only for uspv
testing.  It can send and receive coins.
*/

const (
	keyFileName    = "testkey.hex"
	headerFileName = "headers.bin"
	dbFileName     = "utxo.db"
	// this is my local testnet node, replace it with your own close by.
	// Random internet testnet nodes usually work but sometimes don't, so
	// maybe I should test against different versions out there.
	SPVHostAdr = "127.0.0.1:28333"
)

var (
	Params = &chaincfg.SegNetParams
	SCon   uspv.SPVCon // global here for now
)

func shell() {
	fmt.Printf("LND spv shell v0.0\n")
	fmt.Printf("Not yet well integrated, but soon.\n")

	// read key file (generate if not found)
	rootPriv, err := uspv.ReadKeyFileToECPriv(keyFileName, Params)
	if err != nil {
		log.Fatal(err)
	}
	// setup TxStore first (before spvcon)
	Store := uspv.NewTxStore(rootPriv, Params)
	// setup spvCon

	SCon, err = uspv.OpenSPV(
		SPVHostAdr, headerFileName, dbFileName, &Store, true, false, Params)
	if err != nil {
		log.Fatal(err)
	}

	tip, err := SCon.TS.GetDBSyncHeight() // ask for sync height
	if err != nil {
		log.Fatal(err)
	}
	if tip == 0 { // DB has never been used, set to birthday
		tip = 1234 // hardcoded; later base on keyfile date?
		err = SCon.TS.SetDBSyncHeight(tip)
		if err != nil {
			log.Fatal(err)
		}
	}

	// once we're connected, initiate headers sync
	err = SCon.AskForHeaders()
	if err != nil {
		log.Fatal(err)
	}

	// main shell loop
	for {
		// setup reader with max 4K input chars
		reader := bufio.NewReaderSize(os.Stdin, 4000)
		fmt.Printf("LND# ")                 // prompt
		msg, err := reader.ReadString('\n') // input finishes on enter key
		if err != nil {
			log.Fatal(err)
		}

		cmdslice := strings.Fields(msg) // chop input up on whitespace
		if len(cmdslice) < 1 {
			continue // no input, just prompt again
		}
		fmt.Printf("entered command: %s\n", msg) // immediate feedback
		err = Shellparse(cmdslice)
		if err != nil { // only error should be user exit
			log.Fatal(err)
		}
	}
	return
}

// Shellparse parses user input and hands it to command functions if matching
func Shellparse(cmdslice []string) error {
	var err error
	var args []string
	cmd := cmdslice[0]
	if len(cmdslice) > 1 {
		args = cmdslice[1:]
	}
	if cmd == "exit" || cmd == "quit" {
		return fmt.Errorf("User exit")
	}

	// help gives you really terse help.  Just a list of commands.
	if cmd == "help" {
		err = Help(args)
		if err != nil {
			fmt.Printf("help error: %s\n", err)
		}
		return nil
	}

	// adr generates a new address and displays it
	if cmd == "adr" {
		err = Adr(args)
		if err != nil {
			fmt.Printf("adr error: %s\n", err)
		}
		return nil
	}

	// bal shows the current set of utxos, addresses and score
	if cmd == "bal" {
		err = Bal(args)
		if err != nil {
			fmt.Printf("bal error: %s\n", err)
		}
		return nil
	}

	// send sends coins to the address specified
	if cmd == "send" {
		err = Send(args)
		if err != nil {
			fmt.Printf("send error: %s\n", err)
		}
		return nil
	}
	if cmd == "txs" {
		err = Txs(args)
		if err != nil {
			fmt.Printf("txs error: %s\n", err)
		}
		return nil
	}
	if cmd == "blk" {
		err = Blk(args)
		if err != nil {
			fmt.Printf("blk error: %s\n", err)
		}
		return nil
	}
	fmt.Printf("Command not recognized. type help for command list.\n")
	return nil
}

func Txs(args []string) error {
	alltx, err := SCon.TS.GetAllTxs()
	if err != nil {
		return err
	}
	for i, tx := range alltx {
		fmt.Printf("tx %d %s\n", i, uspv.TxToString(tx))
	}
	return nil
}

func Blk(args []string) error {
	if SCon.RBytes == 0 {
		return fmt.Errorf("Can't check block, spv connection broken")
	}
	if len(args) == 0 {
		return fmt.Errorf("must specify height")
	}
	height, err := strconv.ParseInt(args[0], 10, 32)
	if err != nil {
		return err
	}

	// request most recent block just to test
	err = SCon.AskForOneBlock(int32(height))
	if err != nil {
		return err
	}
	return nil
}

// Bal prints out your score.
func Bal(args []string) error {
	if SCon.TS == nil {
		return fmt.Errorf("Can't get balance, spv connection broken")
	}
	fmt.Printf(" ----- Account Balance ----- \n")
	allUtxos, err := SCon.TS.GetAllUtxos()
	if err != nil {
		return err
	}
	var score int64
	for i, u := range allUtxos {
		fmt.Printf("\tutxo %d height %d %s key: %d amt %d\n",
			i, u.AtHeight, u.Op.String(), u.KeyIdx, u.Value)
		score += u.Value
	}
	height, _ := SCon.TS.GetDBSyncHeight()

	for i, a := range SCon.TS.Adrs {
		fmt.Printf("address %d %s\n", i, a.PkhAdr.String())
	}
	fmt.Printf("Total known utxos: %d\n", len(allUtxos))
	fmt.Printf("Total spendable coin: %d\n", score)
	fmt.Printf("DB sync height: %d\n", height)
	return nil
}

// Adr makes a new address.
func Adr(args []string) error {
	a, err := SCon.TS.NewAdr()
	if err != nil {
		return err
	}
	fmt.Printf("made new address %s, %d addresses total\n",
		a.String(), len(SCon.TS.Adrs))

	return nil
}

// Send sends coins.
func Send(args []string) error {
	if SCon.RBytes == 0 {
		return fmt.Errorf("Can't send, spv connection broken")
	}
	// get all utxos from the database
	allUtxos, err := SCon.TS.GetAllUtxos()
	if err != nil {
		return err
	}
	var score int64 // score is the sum of all utxo amounts.  highest score wins.
	// add all the utxos up to get the score
	for _, u := range allUtxos {
		score += u.Value
	}

	// score is 0, cannot unlock 'send coins' acheivement
	if score == 0 {
		return fmt.Errorf("You don't have money.  Work hard.")
	}
	// need args, fail
	if len(args) < 2 {
		return fmt.Errorf("need args: ssend amount(satoshis) address")
	}
	amt, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return err
	}
	if amt < 1000 {
		return fmt.Errorf("can't send %d, too small", amt)
	}
	adr, err := btcutil.DecodeAddress(args[1], SCon.TS.Param)
	if err != nil {
		fmt.Printf("error parsing %s as address\t", args[1])
		return err
	}
	fmt.Printf("send %d to address: %s \n",
		amt, adr.String())
	err = SendCoins(SCon, adr, amt)
	if err != nil {
		return err
	}
	return nil
}

// SendCoins does send coins, but it's very rudimentary
func SendCoins(s uspv.SPVCon, adr btcutil.Address, sendAmt int64) error {
	var err error
	var score int64
	allUtxos, err := s.TS.GetAllUtxos()
	if err != nil {
		return err
	}

	for _, utxo := range allUtxos {
		score += utxo.Value
	}
	// important rule in bitcoin, output total > input total is invalid.
	if sendAmt > score {
		return fmt.Errorf("trying to send %d but %d available.",
			sendAmt, score)
	}

	tx := wire.NewMsgTx() // make new tx
	// make address script 76a914...88ac
	adrScript, err := txscript.PayToAddrScript(adr)
	if err != nil {
		return err
	}
	// make user specified txout and add to tx
	txout := wire.NewTxOut(sendAmt, adrScript)
	tx.AddTxOut(txout)

	nokori := sendAmt // nokori is how much is needed on input side
	for _, utxo := range allUtxos {
		// generate pkscript to sign
		prevPKscript, err := txscript.PayToAddrScript(
			s.TS.Adrs[utxo.KeyIdx].PkhAdr)
		if err != nil {
			return err
		}
		// make new input from this utxo
		thisInput := wire.NewTxIn(&utxo.Op, prevPKscript)
		tx.AddTxIn(thisInput)
		nokori -= utxo.Value
		if nokori < -10000 { // minimum overage / fee is 1K now
			break
		}
	}
	// there's enough left to make a change output
	if nokori < -200000 {
		change, err := s.TS.NewAdr()
		if err != nil {
			return err
		}

		changeScript, err := txscript.PayToAddrScript(change)
		if err != nil {
			return err
		}
		changeOut := wire.NewTxOut((-100000)-nokori, changeScript)
		tx.AddTxOut(changeOut)
	}

	// use txstore method to sign
	err = s.TS.SignThis(tx)
	if err != nil {
		return err
	}

	fmt.Printf("tx: %s", uspv.TxToString(tx))
	buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
	tx.Serialize(buf)
	fmt.Printf("tx: %x\n", buf.Bytes())

	// send it out on the wire.  hope it gets there.
	// we should deal with rejects.  Don't yet.
	err = s.NewOutgoingTx(tx)
	if err != nil {
		return err
	}
	return nil
}

func Help(args []string) error {
	fmt.Printf("commands:\n")
	fmt.Printf("help adr bal send exit\n")
	return nil
}
