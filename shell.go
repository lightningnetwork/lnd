package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

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

func shell(SPVHostAdr string, Params *chaincfg.Params) {
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
		tip = 21900 // hardcoded; later base on keyfile date?
		err = SCon.TS.SetDBSyncHeight(tip)
		if err != nil {
			log.Fatal(err)
		}
	}

	//	 once we're connected, initiate headers sync
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
	if cmd == "fan" {
		err = Fan(args)
		if err != nil {
			fmt.Printf("fan error: %s\n", err)
		}
		return nil
	}
	if cmd == "sweep" {
		err = Sweep(args)
		if err != nil {
			fmt.Printf("sweep error: %s\n", err)
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
	rawUtxos, err := SCon.TS.GetAllUtxos()
	if err != nil {
		return err
	}
	var allUtxos uspv.SortableUtxoSlice
	for _, utxo := range rawUtxos {
		allUtxos = append(allUtxos, *utxo)
	}
	// smallest and unconfirmed last (because it's reversed)
	sort.Sort(sort.Reverse(allUtxos))

	var score, confScore int64
	for i, u := range allUtxos {
		fmt.Printf("\tutxo %d height %d %s key:%d amt %d",
			i, u.AtHeight, u.Op.String(), u.KeyIdx, u.Value)
		if u.IsWit {
			fmt.Printf(" WIT")
		}
		fmt.Printf("\n")
		score += u.Value
		if u.AtHeight != 0 {
			confScore += u.Value
		}
	}
	height, _ := SCon.TS.GetDBSyncHeight()

	atx, err := SCon.TS.GetAllTxs()

	stxos, err := SCon.TS.GetAllStxos()

	for i, a := range SCon.TS.Adrs {
		wa, err := btcutil.NewAddressWitnessPubKeyHash(
			a.PkhAdr.ScriptAddress(), Params)
		if err != nil {
			return err
		}
		fmt.Printf("address %d %s OR %s\n", i, a.PkhAdr.String(), wa.String())
	}
	fmt.Printf("Total known txs: %d\n", len(atx))
	fmt.Printf("Known utxos: %d\tPreviously spent txos: %d\n",
		len(allUtxos), len(stxos))
	fmt.Printf("Total coin: %d confirmed: %d\n", score, confScore)
	fmt.Printf("DB sync height: %d\n", height)
	return nil
}

// Adr makes a new address.
func Adr(args []string) error {

	// if there's an arg, make 10 adrs
	if len(args) > 0 {
		for i := 0; i < 10; i++ {
			_, err := SCon.TS.NewAdr()
			if err != nil {
				return err
			}
		}
	}
	if len(args) > 1 {
		for i := 0; i < 1000; i++ {
			_, err := SCon.TS.NewAdr()
			if err != nil {
				return err
			}
		}
	}

	// always make one
	a, err := SCon.TS.NewAdr()
	if err != nil {
		return err
	}
	fmt.Printf("made new address %s\n",
		a.String())

	return nil
}

// Sweep sends every confirmed uxto in your wallet to an address.
// it does them all individually to there are a lot of txs generated.
// syntax: sweep adr
func Sweep(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("sweep syntax: sweep adr")
	}

	adr, err := btcutil.DecodeAddress(args[0], SCon.TS.Param)
	if err != nil {
		fmt.Printf("error parsing %s as address\t", args[0])
		return err
	}
	numTxs, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return err
	}
	if numTxs < 1 {
		return fmt.Errorf("can't send %d txs", numTxs)
	}

	rawUtxos, err := SCon.TS.GetAllUtxos()
	if err != nil {
		return err
	}
	var allUtxos uspv.SortableUtxoSlice
	for _, utxo := range rawUtxos {
		allUtxos = append(allUtxos, *utxo)
	}
	// smallest and unconfirmed last (because it's reversed)
	sort.Sort(sort.Reverse(allUtxos))

	for i, u := range allUtxos {
		if u.AtHeight != 0 {
			err = SCon.SendOne(allUtxos[i], adr)
			if err != nil {
				return err
			}
			numTxs--
			if numTxs == 0 {
				return nil
			}
		}
	}

	fmt.Printf("spent all confirmed utxos; not enough by %d\n", numTxs)

	return nil
}

// Fan generates a bunch of fanout.  Only for testing, can be expensive.
// syntax: fan adr numOutputs valOutputs witty
func Fan(args []string) error {
	if len(args) < 3 {
		return fmt.Errorf("fan syntax: fan adr numOutputs valOutputs")
	}
	adr, err := btcutil.DecodeAddress(args[0], SCon.TS.Param)
	if err != nil {
		fmt.Printf("error parsing %s as address\t", args[0])
		return err
	}
	numOutputs, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return err
	}
	valOutputs, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil {
		return err
	}

	adrs := make([]btcutil.Address, numOutputs)
	amts := make([]int64, numOutputs)

	for i := int64(0); i < numOutputs; i++ {
		adrs[i] = adr
		amts[i] = valOutputs + i
	}
	return SCon.SendCoins(adrs, amts)
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
		return fmt.Errorf("need args: ssend address amount(satoshis) wit?")
	}
	adr, err := btcutil.DecodeAddress(args[0], SCon.TS.Param)
	if err != nil {
		fmt.Printf("error parsing %s as address\t", args[0])
		return err
	}
	amt, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return err
	}
	if amt < 1000 {
		return fmt.Errorf("can't send %d, too small", amt)
	}

	fmt.Printf("send %d to address: %s \n",
		amt, adr.String())

	var adrs []btcutil.Address
	var amts []int64

	adrs = append(adrs, adr)
	amts = append(amts, amt)
	err = SCon.SendCoins(adrs, amts)
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
