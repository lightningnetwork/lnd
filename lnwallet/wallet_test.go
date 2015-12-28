package lnwallet

import (
	"bytes"
	"crypto/sha256"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/btcsuite/btcwallet/walletdb"
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

	// Use a hard-coded HD seed.
	testHdSeed = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
	zeroHash = bytes.Repeat([]byte{0}, 32)
)

// assertProperBalance asserts than the total value of the unspent outputs
// within the wallet are *exactly* amount. If unable to retrieve the current
// balance, or the assertion fails, the test will halt with a fatal error.
func assertProperBalance(t *testing.T, lw *LightningWallet, numConfirms, amount int32) {
	balance, err := lw.wallet.TxStore.Balance(0, 20)
	if err != nil {
		t.Fatalf("unable to query for balance: %v", err)
	}
	if balance != btcutil.Amount(amount*1e8) {
		t.Fatalf("wallet credits not properly loaded, should have 20BTC, "+
			"instead have %v", balance)
	}
}

// bobNode represents the other party involved as a node within LN. Bob is our
// only "default-route", we have a direct connection with him.
type bobNode struct {
	privKey *btcec.PrivateKey

	// For simplicity, used for both the commit tx and the multi-sig output.
	channelKey      *btcec.PublicKey
	deliveryAddress btcutil.Address
	revocation      [wire.HashSize]byte
	delay           uint32
	id              [wire.HashSize]byte

	availableOutputs []*wire.TxIn
	changeOutputs    []*wire.TxOut
}

func (b *bobNode) Contribution() *ChannelContribution {
	return &ChannelContribution{
		Inputs:          b.availableOutputs,
		ChangeOutputs:   b.changeOutputs,
		MultiSigKey:     b.channelKey,
		CommitKey:       b.channelKey,
		DeliveryAddress: b.deliveryAddress,
		RevocationHash:  b.revocation,
		CsvDelay:        b.delay,
	}
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

	// Bob's initial revocation hash is just his private key with the first
	// byte changed...
	var revocation [wire.HashSize]byte
	copy(revocation[:], bobsPrivKey)
	revocation[0] = 0xff

	// His ID is just as creative...
	var id [wire.HashSize]byte
	id[0] = 0xff

	return &bobNode{
		id:               id,
		privKey:          privKey,
		channelKey:       pubKey,
		deliveryAddress:  bobAddr,
		revocation:       revocation,
		delay:            5,
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

	wallet, err := NewLightningWallet(privPass, nil, testHdSeed[:], tempTestDir)
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

func testBasicWalletReservationWorkFlow(lnwallet *LightningWallet, t *testing.T) {
	// Create our test wallet, will have a total of 20 BTC available for
	bobNode, err := newBobNode()
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Bob initiates a channel funded with 5 BTC for each side, so 10
	// BTC total. He also generates 2 BTC in change.
	fundingAmount := btcutil.Amount(5 * 1e8)
	// TODO(roasbeef): include csv delay?
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, bobNode.id)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// The channel reservation should now be populated with a multi-sig key
	// from our HD chain, a change output with 3 BTC, and 2 outputs selected
	// of 4 BTC each.
	ourContribution := chanReservation.OurContribution()
	if len(ourContribution.Inputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(ourContribution.Inputs))
	}
	if ourContribution.ChangeOutputs[0].Value != 3e8 {
		t.Fatalf("coin selection failed, change output should be 3e8 "+
			"satoshis, is instead %v", ourContribution.ChangeOutputs[0].Value)
	}
	if ourContribution.MultiSigKey == nil {
		t.Fatalf("alice's key for multi-sig not found")
	}
	if ourContribution.CommitKey == nil {
		t.Fatalf("alice's key for commit not found")
	}
	if ourContribution.DeliveryAddress == nil {
		t.Fatalf("alice's final delivery address not found")
	}
	if bytes.Equal(ourContribution.RevocationHash[:], zeroHash) {
		t.Fatalf("alice's revocation hash not found")
	}
	// TODO(roasbeef):
	//if ourContribution.CsvDelay == 0 {
	//	t.Fatalf("csv delay not set")
	//}

	// Bob sends over his output, change addr, pub keys, initial revocation,
	// final delivery address, and his accepted csv delay for the commitmen
	// t transactions.
	if err := chanReservation.ProcessContribution(bobNode.Contribution()); err != nil {
		t.Fatalf("unable to add bob's funds to the funding tx: %v", err)
	}

	// At this point, the reservation should have our signatures, and a
	// partial funding transaction (missing bob's sigs).
	theirContribution := chanReservation.TheirContribution()
	ourFundingSigs, ourCommitSig := chanReservation.OurSignatures()
	if len(ourFundingSigs) != 2 {
		t.Fatalf("only %v of our sigs present, should have 2",
			len(ourFundingSigs))
	}
	if ourCommitSig == nil {
		t.Fatalf("commitment sig not found")
	}
	// Additionally, the funding tx should have been populated.
	if chanReservation.partialState.FundingTx == nil {
		t.Fatalf("funding transaction never created!")
	}
	// Their funds should also be filled in.
	if len(theirContribution.Inputs) != 1 {
		t.Fatalf("bob's outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(theirContribution.Inputs))
	}
	if theirContribution.ChangeOutputs[0].Value != 2e8 {
		t.Fatalf("bob should have one change output with value 2e8"+
			"satoshis, is instead %v",
			theirContribution.ChangeOutputs[0].Value)
	}
	if theirContribution.MultiSigKey == nil {
		t.Fatalf("bob's key for multi-sig not found")
	}
	if theirContribution.CommitKey == nil {
		t.Fatalf("bob's key for commit tx not found")
	}
	if theirContribution.DeliveryAddress == nil {
		t.Fatalf("bob's final delivery address not found")
	}
	if bytes.Equal(theirContribution.RevocationHash[:], zeroHash) {
		t.Fatalf("bob's revocaiton hash not found")
	}

	// Alice responds with her output, change addr, multi-sig key and signatures.
	// Bob then responds with his signatures.
	bobsSigs, err := bobNode.signFundingTx(chanReservation.partialState.FundingTx)
	if err != nil {
		t.Fatalf("unable to sign inputs for bob: %v", err)
	}
	fakeCommitSig := bytes.Repeat([]byte{1}, 64)
	if err := chanReservation.CompleteReservation(bobsSigs, fakeCommitSig); err != nil {
		t.Fatalf("unable to complete funding tx: %v", err)
	}

	// At this point, the channel can be considered "open" when the funding
	// txn hits a "comfortable" depth.

	fundingTx := chanReservation.FinalFundingTx()

	// The resulting active channel state should have been persisted to the DB>
	channel, err := lnwallet.channelDB.FetchOpenChannel(bobNode.id)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if channel.FundingTx.TxSha() != fundingTx.TxSha() {
		t.Fatalf("channel state not properly saved")
	}

	// The funding tx should now be valid and complete.
	// Check each input and ensure all scripts are fully valid.
	// TODO(roasbeef): remove this loop after nodetest hooked up.
	var zeroHash wire.ShaHash
	for i, input := range fundingTx.TxIn {
		var pkscript []byte
		// Bob's txin
		if bytes.Equal(input.PreviousOutPoint.Hash.Bytes(),
			zeroHash.Bytes()) {
			pkscript = bobNode.changeOutputs[0].PkScript
		} else {
			// Does the wallet know about the txin?
			txDetail, err := lnwallet.wallet.TxStore.TxDetails(&input.PreviousOutPoint.Hash)
			if txDetail == nil || err != nil {
				t.Fatalf("txstore can't find tx detail, err: %v", err)
			}
			prevIndex := input.PreviousOutPoint.Index
			pkscript = txDetail.TxRecord.MsgTx.TxOut[prevIndex].PkScript
		}

		vm, err := txscript.NewEngine(pkscript,
			fundingTx, i, txscript.StandardVerifyFlags, nil)
		if err != nil {
			// TODO(roasbeef): cancel at this stage if invalid sigs?
			t.Fatalf("cannot create script engine: %s", err)
		}
		if err = vm.Execute(); err != nil {
			t.Fatalf("cannot validate transaction: %s", err)
		}
	}
}

func testFundingTransactionLockedOutputs(lnwallet *LightningWallet, t *testing.T) {
	// Create two channels, both asking for 8 BTC each, totalling 16
	// BTC.
	// TODO(roasbeef): tests for concurrent funding.
	//  * also func for below
	fundingAmount := btcutil.Amount(8 * 1e8)
	chanReservation1, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 1: %v", err)
	}
	chanReservation2, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 2: %v", err)
	}

	// Neither should have any change, as all our output sizes are
	// identical (4BTC).
	ourContribution1 := chanReservation1.OurContribution()
	if len(ourContribution1.Inputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, has %v "+
			"outputs should have 2", len(ourContribution1.Inputs))
	}
	if len(ourContribution1.ChangeOutputs) != 0 {
		t.Fatalf("funding transaction should have no change, instead has %v",
			len(ourContribution1.ChangeOutputs))
	}
	ourContribution2 := chanReservation2.OurContribution()
	if len(ourContribution2.Inputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(ourContribution2.Inputs))
	}
	if len(ourContribution2.ChangeOutputs) != 0 {
		t.Fatalf("funding transaction should have no change, instead has %v",
			len(ourContribution2.ChangeOutputs))
	}

	// Now attempt to reserve funds for another channel, this time requesting
	// 5 BTC. We only have 4BTC worth of outpoints that aren't locked, so
	// this should fail.
	amt := btcutil.Amount(8 * 1e8)
	failedReservation, err := lnwallet.InitChannelReservation(amt,
		SIGHASH, testHdSeed)
	if err == nil {
		t.Fatalf("not error returned, should fail on coin selection")
	}
	if err != coinset.ErrCoinsNoSelectionAvailable {
		t.Fatalf("error not coinselect error: %v", err)
	}
	if failedReservation != nil {
		t.Fatalf("reservation should be nil")
	}
}

func testFundingCancellationNotEnoughFunds(lnwallet *LightningWallet, t *testing.T) {
	// Create a reservation for 12 BTC.
	fundingAmount := btcutil.Amount(12 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// There should be three locked outpoints.
	lockedOutPoints := lnwallet.wallet.LockedOutpoints()
	if len(lockedOutPoints) != 3 {
		t.Fatalf("two outpoints should now be locked, instead %v are",
			lockedOutPoints)
	}

	// Attempt to create another channel with 12 BTC, this should fail.
	failedReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed)
	if err != coinset.ErrCoinsNoSelectionAvailable {
		t.Fatalf("coin selection succeded should have insufficient funds: %+v",
			failedReservation)
	}

	// Now cancel that old reservation.
	if err := chanReservation.Cancel(); err != nil {
		t.Fatalf("unable to cancel reservation: %v", err)
	}

	// Those outpoints should no longer be locked.
	lockedOutPoints = lnwallet.wallet.LockedOutpoints()
	if len(lockedOutPoints) != 0 {
		t.Fatalf("outpoints still locked")
	}

	// Reservation ID should now longer be tracked.
	_, ok := lnwallet.fundingLimbo[chanReservation.reservationID]
	if ok {
		t.Fatalf("funding reservation still in map")
	}

	// TODO(roasbeef): create method like Balance that ignores locked
	// outpoints, will let us fail early/fast instead of querying and
	// attempting coin selection.

	// Request to fund a new channel should now succeeed.
	_, err = lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}
}

func testCancelNonExistantReservation(lnwallet *LightningWallet, t *testing.T) {
	// Create our own reservation, give it some ID.
	res := newChannelReservation(SIGHASH, 1000, 5000, lnwallet, 22)

	// Attempt to cancel this reservation. This should fail, we know
	// nothing of it.
	if err := res.Cancel(); err == nil {
		t.Fatalf("cancelled non-existant reservation")
	}

	// Outputs shouuld be available now
	// Reservation should be missing from limbo.
}

func testFundingReservationInvalidCounterpartySigs(lnwallet *LightningWallet, t *testing.T) {
}

func testFundingTransactionTxFees(lnwallet *LightningWallet, t *testing.T) {
}

var walletTests = []func(w *LightningWallet, test *testing.T){
	testBasicWalletReservationWorkFlow,
	testFundingTransactionLockedOutputs,
	testFundingCancellationNotEnoughFunds,
	testFundingReservationInvalidCounterpartySigs,
	testFundingTransactionLockedOutputs,
}

type testLnWallet struct {
	lnwallet    *LightningWallet
	cleanUpFunc func()
}

func clearWalletState(w *LightningWallet) error {
	w.nextFundingID = 0
	w.fundingLimbo = make(map[uint64]*ChannelReservation)
	w.wallet.ResetLockedOutpoints()
	return w.channelDB.Wipe()
}

// TODO(roasbeef): why is wallet so slow to create+open?
// * investigate
// * re-use same lnwallet instance accross tests resetting each time?
func TestLightningWallet(t *testing.T) {
	// funding via 5 outputs with 4BTC each.
	testDir, lnwallet, err := createTestWallet()
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}
	defer os.RemoveAll(testDir)
	defer lnwallet.Stop()

	// The wallet should now have 20BTC available for spending.
	assertProperBalance(t, lnwallet, 1, 20)

	// TODO(roasbeef): initialize nodetest state here once done.

	// Execute every test, clearing possibly mutated wallet state after
	// each step.
	for _, walletTest := range walletTests {
		walletTest(lnwallet, t)

		if err := clearWalletState(lnwallet); err != nil && err != walletdb.ErrBucketNotFound {
			t.Fatalf("unable to clear wallet state: %v", err)
		}
	}
}
