package lnwallet

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/lightningnetwork/lnd/channeldb"

	"github.com/Roasbeef/btcd/rpctest"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcutil/coinset"
	"github.com/btcsuite/btcwallet/waddrmgr"
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
	balance, err := lw.CalculateBalance(1)
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
	revocation      [20]byte
	delay           uint32
	id              [wire.HashSize]byte

	availableOutputs []*wire.TxIn
	changeOutputs    []*wire.TxOut
}

// Contribution returns bobNode's contribution necessary to open a payment
// channel with Alice.
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
// NOTE: This generates the full sig-script.
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

// signCommitTx generates a raw signature required for generating a spend from
// the funding transaction.
func (b *bobNode) signCommitTx(commitTx *wire.MsgTx, fundingScript []byte) ([]byte, error) {
	return txscript.RawTxInSignature(commitTx, 0, fundingScript,
		txscript.SigHashAll, b.privKey)
}

// newBobNode generates a test "ln node" to interact with Alice (us). For the
// funding transaction, bob has a single output totaling 7BTC. For our basic
// test, he'll fund the channel with 5BTC, leaving 2BTC to the change output.
// TODO(roasbeef): proper handling of change etc.
func newBobNode(miner *rpctest.Harness) (*bobNode, error) {
	// First, parse Bob's priv key in order to obtain a key he'll use for the
	// multi-sig funding transaction.
	privKey, pubKey := btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)

	// Next, generate an output redeemable by bob.
	bobAddrPk, err := btcutil.NewAddressPubKey(privKey.PubKey().SerializeCompressed(),
		miner.ActiveNet)
	if err != nil {
		return nil, err
	}
	bobAddr := bobAddrPk.AddressPubKeyHash()
	bobAddrScript, err := txscript.PayToAddrScript(bobAddr)
	if err != nil {
		return nil, err
	}

	// Give bobNode one 7 BTC output for use in creating channels.
	output := &wire.TxOut{7e8, bobAddrScript}
	mainTxid, err := miner.CoinbaseSpend([]*wire.TxOut{output})
	if err != nil {
		return nil, err
	}

	// Mine a block in order to include the above output in a block. During
	// the reservation workflow, we currently test to ensure that the funding
	// output we're given actually exists.
	if _, err := miner.Node.Generate(1); err != nil {
		return nil, err
	}

	// Grab the transaction in order to locate the output index to Bob.
	tx, err := miner.Node.GetRawTransaction(mainTxid)
	if err != nil {
		return nil, err
	}
	found, index := findScriptOutputIndex(tx.MsgTx(), bobAddrScript)
	if !found {
		return nil, fmt.Errorf("output to bob never created")
	}

	prevOut := wire.NewOutPoint(mainTxid, index)
	// TODO(roasbeef): When the chain rpc is hooked in, assert bob's output
	// actually exists and it unspent in the chain.
	bobTxIn := wire.NewTxIn(prevOut, nil, nil)

	// Using bobs priv key above, create a change output he can spend.
	bobChangeOutput := wire.NewTxOut(2*1e8, bobAddrScript)

	// Bob's initial revocation hash is just his private key with the first
	// byte changed...
	var revocation [20]byte
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

func loadTestCredits(miner *rpctest.Harness, w *LightningWallet, numOutputs, btcPerOutput int) error {
	// Using the mining node, spend from a coinbase output numOutputs to
	// give us btcPerOutput with each output.
	satoshiPerOutput := int64(btcPerOutput * 1e8)
	addrs := make([]btcutil.Address, 0, numOutputs)
	for i := 0; i < numOutputs; i++ {
		// Grab a fresh address from the wallet to house this output.
		walletAddr, err := w.NewAddress(waddrmgr.DefaultAccountNum)
		if err != nil {
			return err
		}

		script, err := txscript.PayToAddrScript(walletAddr)
		if err != nil {
			return err
		}

		addrs = append(addrs, walletAddr)

		output := &wire.TxOut{satoshiPerOutput, script}
		if _, err := miner.CoinbaseSpend([]*wire.TxOut{output}); err != nil {
			return err
		}
	}

	// TODO(roasbeef): shouldn't hardcode 10, use config param that dictates
	// how many confs we wait before opening a channel.
	// Generate 10 blocks with the mining node, this should mine all
	// numOutputs transactions created above. We generate 10 blocks here
	// in order to give all the outputs a "sufficient" number of confirmations.
	if _, err := miner.Node.Generate(10); err != nil {
		return err
	}

	_, bestHeight, err := miner.Node.GetBestBlock()
	if err != nil {
		return err
	}

	// Wait until the wallet has finished syncing up to the main chain.
	ticker := time.NewTicker(100 * time.Millisecond)
out:
	for {
		select {
		case <-ticker.C:
			if w.Manager.SyncedTo().Height == bestHeight {
				break out
			}
		}
	}
	ticker.Stop()

	// Trigger a re-scan to ensure the wallet knows of the newly created
	// outputs it can spend.
	if err := w.Rescan(addrs, nil); err != nil {
		return err
	}

	return nil
}

// createTestWallet creates a test LightningWallet will a total of 20BTC
// available for funding channels.
func createTestWallet(miningNode *rpctest.Harness, netParams *chaincfg.Params) (string, *LightningWallet, error) {
	privPass := []byte("private-test")
	tempTestDir, err := ioutil.TempDir("", "lnwallet")
	if err != nil {
		return "", nil, nil
	}

	rpcConfig := miningNode.RPCConfig()
	config := &Config{
		PrivatePass: privPass,
		HdSeed:      testHdSeed[:],
		DataDir:     tempTestDir,
		NetParams:   netParams,
		RpcHost:     rpcConfig.Host,
		RpcUser:     rpcConfig.User,
		RpcPass:     rpcConfig.Pass,
		CACert:      rpcConfig.Certificates,
	}

	dbDir := filepath.Join(tempTestDir, "cdb")
	cdb, err := channeldb.Create(dbDir)
	if err != nil {
		return "", nil, err
	}

	wallet, err := NewLightningWallet(config, cdb)
	if err != nil {
		return "", nil, err
	}
	if err := wallet.Startup(); err != nil {
		return "", nil, err
	}

	cdb.RegisterCryptoSystem(&WaddrmgrEncryptorDecryptor{wallet.Manager})

	// Load our test wallet with 5 outputs each holding 4BTC.
	if err := loadTestCredits(miningNode, wallet, 5, 4); err != nil {
		return "", nil, err
	}

	return tempTestDir, wallet, nil
}

func testBasicWalletReservationWorkFlow(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create our test wallet, will have a total of 20 BTC available for
	bobNode, err := newBobNode(miner)
	if err != nil {
		t.Fatalf("unable to create bob node: %v", err)
	}

	// Bob initiates a channel funded with 5 BTC for each side, so 10
	// BTC total. He also generates 2 BTC in change.
	fundingAmount := btcutil.Amount(5 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, bobNode.id, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// The channel reservation should now be populated with a multi-sig key
	// from our HD chain, a change output with 3 BTC, and 2 outputs selected
	// of 4 BTC each. Additionally, the rest of the items needed to fufill a
	// funding contribution should also have been filled in.
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
	if ourContribution.CsvDelay == 0 {
		t.Fatalf("csv delay not set")
	}

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
	commitSig, err := bobNode.signCommitTx(
		chanReservation.partialState.OurCommitTx,
		chanReservation.partialState.FundingRedeemScript)
	if err != nil {
		t.Fatalf("bob is unable to sign alice's commit tx: %v", err)
	}
	if err := chanReservation.CompleteReservation(bobsSigs, commitSig); err != nil {
		t.Fatalf("unable to complete funding tx: %v", err)
	}

	// At this point, the channel can be considered "open" when the funding
	// txn hits a "comfortable" depth.

	// The resulting active channel state should have been persisted to the DB.
	fundingTx := chanReservation.FinalFundingTx()
	channel, err := lnwallet.channelDB.FetchOpenChannel(bobNode.id)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if channel.FundingTx.TxSha() != fundingTx.TxSha() {
		t.Fatalf("channel state not properly saved")
	}
}

func testFundingTransactionLockedOutputs(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create two channels, both asking for 8 BTC each, totalling 16
	// BTC.
	// TODO(roasbeef): tests for concurrent funding.
	//  * also func for below
	fundingAmount := btcutil.Amount(8 * 1e8)
	chanReservation1, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 1: %v", err)
	}
	chanReservation2, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed, 4)
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
		SIGHASH, testHdSeed, 4)
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

func testFundingCancellationNotEnoughFunds(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create a reservation for 12 BTC.
	fundingAmount := btcutil.Amount(12 * 1e8)
	chanReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// There should be three locked outpoints.
	lockedOutPoints := lnwallet.LockedOutpoints()
	if len(lockedOutPoints) != 3 {
		t.Fatalf("two outpoints should now be locked, instead %v are",
			lockedOutPoints)
	}

	// Attempt to create another channel with 12 BTC, this should fail.
	failedReservation, err := lnwallet.InitChannelReservation(fundingAmount,
		SIGHASH, testHdSeed, 4)
	if err != coinset.ErrCoinsNoSelectionAvailable {
		t.Fatalf("coin selection succeded should have insufficient funds: %+v",
			failedReservation)
	}

	// Now cancel that old reservation.
	if err := chanReservation.Cancel(); err != nil {
		t.Fatalf("unable to cancel reservation: %v", err)
	}

	// Those outpoints should no longer be locked.
	lockedOutPoints = lnwallet.LockedOutpoints()
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
		SIGHASH, testHdSeed, 4)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}
}

func testCancelNonExistantReservation(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
	// Create our own reservation, give it some ID.
	res := newChannelReservation(SIGHASH, 1000, 5000, lnwallet, 22)

	// Attempt to cancel this reservation. This should fail, we know
	// nothing of it.
	if err := res.Cancel(); err == nil {
		t.Fatalf("cancelled non-existant reservation")
	}
}

func testFundingReservationInvalidCounterpartySigs(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
}

func testFundingTransactionTxFees(miner *rpctest.Harness, lnwallet *LightningWallet, t *testing.T) {
}

var walletTests = []func(miner *rpctest.Harness, w *LightningWallet, test *testing.T){
	testBasicWalletReservationWorkFlow,
	testFundingTransactionLockedOutputs,
	testFundingCancellationNotEnoughFunds,
	testFundingReservationInvalidCounterpartySigs,
	testFundingTransactionLockedOutputs,
	// TODO(roasbeef):
	// * test for non-existant output given in funding tx
	// * channel open after confirmations
	// * channel update stuff
}

type testLnWallet struct {
	lnwallet    *LightningWallet
	cleanUpFunc func()
}

func clearWalletState(w *LightningWallet) {
	w.nextFundingID = 0
	w.fundingLimbo = make(map[uint64]*ChannelReservation)
	w.ResetLockedOutpoints()
}

func TestLightningWallet(t *testing.T) {
	// TODO(roasbeef): switch to testnetL later
	netParams := &chaincfg.SimNetParams

	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set
	// up this node with a chain length of 125, so we have plentyyy of BTC
	// to play around with.
	miningNode, err := rpctest.New(netParams, nil, nil)
	defer miningNode.TearDown()
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	if err := miningNode.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	// Funding via 5 outputs with 4BTC each.
	testDir, lnwallet, err := createTestWallet(miningNode, netParams)
	if err != nil {
		t.Fatalf("unable to create test ln wallet: %v", err)
	}
	defer os.RemoveAll(testDir)
	defer lnwallet.Shutdown()

	// The wallet should now have 20BTC available for spending.
	assertProperBalance(t, lnwallet, 1, 20)

	// Execute every test, clearing possibly mutated wallet state after
	// each step.
	for _, walletTest := range walletTests {
		walletTest(miningNode, lnwallet, t)

		// TODO(roasbeef): possible reset mining node's chainstate to
		// initial level, cleanly wipe buckets
		clearWalletState(lnwallet)
	}
}
