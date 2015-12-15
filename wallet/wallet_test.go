package wallet

import (
	"crypto/sha256"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/wtxmgr"
)

var (
	privPass = []byte("private-test")

	// For simplicity a single priv key controls all of our test outputs.
	testWalletPrivKey = []byte{
		0x2b, 0xd8, 0x06, 0xc9, 0x7f, 0x0e, 0x00, 0xaf,
		0x1a, 0x1f, 0xc3, 0x32, 0x8f, 0xa7, 0x63, 0xa9,
		0x26, 0x97, 0x23, 0xc8, 0xdb, 0x8f, 0xac, 0x4f,
		0x93, 0xaf, 0x71, 0xdb, 0x18, 0x6d, 0x6e, 0x90,
	}

	// We're alice :)
	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed in order to avoid derivation.
	testHdSeed = []byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
)

// bobNode represents the other party involved as a node within LN. Bob is our
// only "default-route", we have a direct connection with him.
type bobNode struct {
	privKey     *btcec.PrivateKey
	multiSigKey *btcec.PublicKey

	availableOutputs []*wire.TxIn
	changeOutputs    []*wire.TxOut
}

// signFundingTx generates signatures for all the inputs in the funding tx
// belonging to Bob.
func (b *bobNode) signFundingTx(fundingTx *wire.MsgTx) ([][]byte, error) {
	bobSigs := make([][]byte, 0, len(b.availableOutputs))
	bobPkScript := b.changeOutputs[0].PkScript
	for i, _ := range fundingTx.TxIn {
		// Alice has already signed this input
		if fundingTx.TxIn[i].SignatureScript != nil {
			continue
		}

		sigScript, err := txscript.SignatureScript(fundingTx, i,
			bobPkScript, txscript.SigHashAll, b.privKey,
			true)
		if err != nil {
			return nil, err
		}

		bobSigs = append(bobSigs, sigScript)
	}

	return bobSigs, nil
}

// newBobNode generates a test "ln node" to interact with Alice (us). For the
// funding transaction, bob has a single output totaling 7BTC. For our basic
// test, he'll fund the channel with 5BTC, leaving 2BTC to the change output.
// TODO(roasbeef): proper handling of change etc.
func newBobNode() (*bobNode, error) {
	// First, parse Bob's priv key in order to obtain a key he'll use for the
	// multi-sig funding transaction.
	privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)

	// Next, generate an output redeemable by bob.
	bobAddr, err := btcutil.NewAddressPubKey(privKey.PubKey().SerializeCompressed(),
		ActiveNetParams)
	if err != nil {
		return nil, err
	}
	bobAddrScript, err := txscript.PayToAddrScript(bobAddr.AddressPubKeyHash())
	if err != nil {
		return nil, err
	}
	prevOut := wire.NewOutPoint(&wire.ShaHash{}, ^uint32(0))
	// TODO(roasbeef): When the chain rpc is hooked in, assert bob's output
	// actually exists and it unspent in the chain.
	bobTxIn := wire.NewTxIn(prevOut, nil)

	// Using bobs priv key above, create a change address he can spend.
	bobChangeOutput := wire.NewTxOut(2*1e8, bobAddrScript)

	return &bobNode{
		privKey:          privKey,
		multiSigKey:      pubKey,
		availableOutputs: []*wire.TxIn{bobTxIn},
		changeOutputs:    []*wire.TxOut{bobChangeOutput},
	}, nil
}

// addTestTx adds a output spendable by our test wallet, marked as included in
// 'block'.
func addTestTx(w *LightningWallet, rec *wtxmgr.TxRecord, block *wtxmgr.BlockMeta) error {
	err := w.wallet.TxStore.InsertTx(rec, block)
	if err != nil {
		return err
	}

	// Check every output to determine whether it is controlled by a wallet
	// key.  If so, mark the output as a credit.
	for i, output := range rec.MsgTx.TxOut {
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(output.PkScript,
			ActiveNetParams)
		if err != nil {
			// Non-standard outputs are skipped.
			continue
		}
		for _, addr := range addrs {
			ma, err := w.wallet.Manager.Address(addr)
			if err == nil {
				err = w.wallet.TxStore.AddCredit(rec, block, uint32(i),
					ma.Internal())
				if err != nil {
					return err
				}
				err = w.wallet.Manager.MarkUsed(addr)
				if err != nil {
					return err
				}
				continue
			}

			// Missing addresses are skipped.  Other errors should
			// be propagated.
			if !waddrmgr.IsError(err, waddrmgr.ErrAddressNotFound) {
				return err
			}
		}
	}
	return nil
}

func genBlockHash(n int) *wire.ShaHash {
	sha := sha256.Sum256([]byte{byte(n)})
	hash, _ := wire.NewShaHash(sha[:])
	return hash
}

func loadTestCredits(w *LightningWallet, numOutputs, btcPerOutput int) error {
	// Import the priv key (converting to WIF) above that controls all our
	// available outputs.
	privKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), testWalletPrivKey)
	if err := w.wallet.Unlock(privPass, time.Duration(0)); err != nil {
		return err
	}
	bs := &waddrmgr.BlockStamp{Hash: *genBlockHash(1), Height: 1}
	wif, err := btcutil.NewWIF(privKey, ActiveNetParams, true)
	if err != nil {
		return err
	}
	if _, err := w.wallet.ImportPrivateKey(wif, bs, false); err != nil {
		return nil
	}
	if err := w.wallet.Manager.SetSyncedTo(&waddrmgr.BlockStamp{int32(1), *genBlockHash(1)}); err != nil {
		return err
	}

	blk := wtxmgr.BlockMeta{wtxmgr.Block{Hash: *genBlockHash(2), Height: 2}, time.Now()}

	// Create a simple P2PKH pubkey script spendable by Alice. For simplicity
	// all of Alice's spendable funds will reside in this output.
	satosihPerOutput := int64(btcPerOutput * 1e8)
	walletAddr, err := btcutil.NewAddressPubKey(privKey.PubKey().SerializeCompressed(),
		ActiveNetParams)
	if err != nil {
		return err
	}
	walletScriptCredit, err := txscript.PayToAddrScript(walletAddr.AddressPubKeyHash())
	if err != nil {
		return err
	}

	// Create numOutputs outputs spendable by our wallet each holding btcPerOutput
	// in satoshis.
	tx := wire.NewMsgTx()
	prevOut := wire.NewOutPoint(genBlockHash(999), 1)
	txIn := wire.NewTxIn(prevOut, []byte{txscript.OP_0, txscript.OP_0})
	tx.AddTxIn(txIn)
	for i := 0; i < numOutputs; i++ {
		tx.AddTxOut(wire.NewTxOut(satosihPerOutput, walletScriptCredit))
	}
	txCredit, err := wtxmgr.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		return err
	}

	if err := addTestTx(w, txCredit, &blk); err != nil {
		return err
	}
	if err := w.wallet.Manager.SetSyncedTo(&waddrmgr.BlockStamp{int32(2), *genBlockHash(2)}); err != nil {
		return err
	}

	// Make the wallet think it's been synced to block 10. This way the
	// outputs we added above will have sufficient confirmations
	// (hard coded to 6 atm).
	for i := 3; i < 10; i++ {
		sha := *genBlockHash(i)
		if err := w.wallet.Manager.SetSyncedTo(&waddrmgr.BlockStamp{int32(i), sha}); err != nil {
			return err
		}
	}

	return nil
}

// createTestWallet creates a test LightningWallet will a total of 20BTC
// available for funding channels.
func createTestWallet() (string, *LightningWallet, error) {
	privPass := []byte("private-test")
	tempTestDir, err := ioutil.TempDir("", "lnwallet")
	if err != nil {
		return "", nil, nil
	}

	wallet, err := NewLightningWallet(privPass, nil, testHdSeed, tempTestDir)
	if err != nil {
		return "", nil, err
	}
	wallet.Start()

	// Load our test wallet with 5 outputs each holding 4BTC.
	if err := loadTestCredits(wallet, 5, 4); err != nil {
		return "", nil, err
	}

	return tempTestDir, wallet, nil
}

func TestBasicWalletReservationWorkFlow(t *testing.T) {
	// Create our test wallet, will have a total of 20 BTC available for
	// funding via 5 outputs with 4BTC each.
	testDir, lnwallet, err := createTestWallet()
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}
	defer os.RemoveAll(testDir)
	defer lnwallet.Stop()

	// The wallet should now have 20BTC available for spending.
	balance, err := lnwallet.wallet.TxStore.Balance(0, 20)
	if err != nil {
		t.Fatalf("unable to query for balance: %v", err)
	}
	if balance != btcutil.Amount(20*1e8) {
		t.Fatalf("wallet credits not properly loaded, should have 20BTC, "+
			"instead have %v", balance)
	}

	bobNode, err := newBobNode()
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Bob initiates a channel funded with 5 BTC for each side, so 10
	// BTC total. He also generates 2 BTC in change.
	fundingAmount := btcutil.Amount(5 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount, SIGHASH)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// The channel reservation should now be populated with a multi-sig key
	// from our HD chain, a change output with 3 BTC, and 2 outputs selected
	// of 4 BTC each.
	ourInputs, ourChange, ourMultsigKey := chanReservation.OurFunds()
	if len(ourInputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(ourInputs))
	}
	if ourMultsigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if ourChange[0].Value != 3e8 {
		t.Fatalf("coin selection failed, change output should be 3e8 "+
			"satoshis, is instead %v", ourChange[0].Value)
	}

	// Bob sends over his output, change addr and multi-sig key.
	if err := chanReservation.AddFunds(bobNode.availableOutputs,
		bobNode.changeOutputs, bobNode.multiSigKey); err != nil {
		t.Fatalf("unable to add bob's funds to the funding tx: %v", err)
	}

	// At this point, the reservation should have our signatures, and a
	// partial funding transaction (missing bob's sigs).
	if len(chanReservation.OurSigs()) != 2 {
		t.Fatalf("only %v of our sigs present, should have 2",
			len(chanReservation.OurSigs()))
	}
	// Additionally, the funding tx should have been populated.
	if chanReservation.fundingTx == nil {
		t.Fatalf("funding transaction never created!")
	}
	// Their funds should also be filled in.
	theirInputs, theirChange, theirMultsigKey := chanReservation.TheirFunds()
	if len(theirInputs) != 1 {
		t.Fatalf("bob's outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(theirInputs))
	}
	if theirMultsigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if theirChange[0].Value != 2e8 {
		t.Fatalf("bob should have one change output with value 2e8"+
			"satoshis, is instead %v", theirChange[0].Value)
	}

	// Alice responds with her output, change addr, multi-sig key and signatures.
	// Bob then responds with his signatures.
	bobsSigs, err := bobNode.signFundingTx(chanReservation.fundingTx)
	if err != nil {
		t.Fatalf("unable to sign inputs for bob: %v", err)
	}
	if err := chanReservation.CompleteReservation(bobsSigs); err != nil {
		t.Fatalf("unable to complete funding tx: %v", err)
	}

	// TODO(roasbeef):
	// * verify funding tx commited to disk
	// * all sigs valid for all inputs

	// The funding tx should now be valid and complete.
}

func TestFundingTransactionTxFees(t *testing.T) {
}

func TestFundingTransactionLockedOutputs(t *testing.T) {
}

func TestFundingTransactionCancellationFreeOutputs(t *testing.T) {
}

func TestFundingReservationInsufficientFunds(t *testing.T) {
}

func TestFundingReservationInvalidCounterpartySigs(t *testing.T) {
}
