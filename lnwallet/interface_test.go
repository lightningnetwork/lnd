package lnwallet_test

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/boltdb/bolt"

	"github.com/roasbeef/btcwallet/chain"

	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainntnfs/btcdnotify"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/roasbeef/btcd/chaincfg"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	_ "github.com/roasbeef/btcwallet/walletdb/bdb"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/integration/rpctest"
	"github.com/roasbeef/btcd/txscript"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
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

	bobsPrivKey = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Use a hard-coded HD seed.
	testHdSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	aliceHDSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x18, 0xa3, 0xef, 0xb9,
		0x64, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}
	bobHDSeed = chainhash.Hash{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x98, 0xa3, 0xef, 0xb9,
		0x69, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	netParams = &chaincfg.SimNetParams
	chainHash = netParams.GenesisHash

	_, alicePub = btcec.PrivKeyFromBytes(btcec.S256(), testHdSeed[:])
	_, bobPub   = btcec.PrivKeyFromBytes(btcec.S256(), bobsPrivKey)

	// The number of confirmations required to consider any created channel
	// open.
	numReqConfs uint16 = 1

	csvDelay uint16 = 4

	bobAddr, _   = net.ResolveTCPAddr("tcp", "10.0.0.2:9000")
	aliceAddr, _ = net.ResolveTCPAddr("tcp", "10.0.0.3:9000")
)

// assertProperBalance asserts than the total value of the unspent outputs
// within the wallet are *exactly* amount. If unable to retrieve the current
// balance, or the assertion fails, the test will halt with a fatal error.
func assertProperBalance(t *testing.T, lw *lnwallet.LightningWallet, numConfirms int32, amount int64) {
	balance, err := lw.ConfirmedBalance(numConfirms, false)
	if err != nil {
		t.Fatalf("unable to query for balance: %v", err)
	}
	if balance != btcutil.Amount(amount*1e8) {
		t.Fatalf("wallet credits not properly loaded, should have 40BTC, "+
			"instead have %v", balance)
	}
}

func assertChannelOpen(t *testing.T, miner *rpctest.Harness, numConfs uint32,
	c <-chan *lnwallet.LightningChannel) *lnwallet.LightningChannel {
	// Mine a single block. After this block is mined, the channel should
	// be considered fully open.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	select {
	case lnc := <-c:
		return lnc
	case <-time.After(time.Second * 5):
		t.Fatalf("channel never opened")
		return nil
	}
}

func assertReservationDeleted(res *lnwallet.ChannelReservation, t *testing.T) {
	if err := res.Cancel(); err == nil {
		t.Fatalf("reservation wasn't deleted from wallet")
	}
}

// calcStaticFee calculates appropriate fees for commitment transactions.  This
// function provides a simple way to allow test balance assertions to take fee
// calculations into account.
// TODO(bvu): Refactor when dynamic fee estimation is added.
func calcStaticFee(numHTLCs int) btcutil.Amount {
	const (
		commitWeight = btcutil.Amount(724)
		htlcWeight   = 172
		feePerKw     = btcutil.Amount(250/4) * 1000
	)
	return feePerKw * (commitWeight +
		btcutil.Amount(htlcWeight*numHTLCs)) / 1000
}

func loadTestCredits(miner *rpctest.Harness, w *lnwallet.LightningWallet,
	numOutputs, btcPerOutput int) error {

	// Using the mining node, spend from a coinbase output numOutputs to
	// give us btcPerOutput with each output.
	satoshiPerOutput := int64(btcPerOutput * 1e8)
	addrs := make([]btcutil.Address, 0, numOutputs)
	for i := 0; i < numOutputs; i++ {
		// Grab a fresh address from the wallet to house this output.
		walletAddr, err := w.NewAddress(lnwallet.WitnessPubKey, false)
		if err != nil {
			return err
		}

		script, err := txscript.PayToAddrScript(walletAddr)
		if err != nil {
			return err
		}

		addrs = append(addrs, walletAddr)

		output := &wire.TxOut{
			Value:    satoshiPerOutput,
			PkScript: script,
		}
		if _, err := miner.SendOutputs([]*wire.TxOut{output}, 10); err != nil {
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

	// Wait until the wallet has finished syncing up to the main chain.
	ticker := time.NewTicker(100 * time.Millisecond)
	expectedBalance := btcutil.Amount(satoshiPerOutput * int64(numOutputs))

	for range ticker.C {
		balance, err := w.ConfirmedBalance(1, false)
		if err != nil {
			return err
		}
		if balance == expectedBalance {
			break
		}
	}
	ticker.Stop()

	return nil
}

// createTestWallet creates a test LightningWallet will a total of 20BTC
// available for funding channels.
func createTestWallet(tempTestDir string, miningNode *rpctest.Harness,
	netParams *chaincfg.Params, notifier chainntnfs.ChainNotifier,
	wc lnwallet.WalletController, signer lnwallet.Signer,
	bio lnwallet.BlockChainIO) (*lnwallet.LightningWallet, error) {

	dbDir := filepath.Join(tempTestDir, "cdb")
	cdb, err := channeldb.Open(dbDir)
	if err != nil {
		return nil, err
	}

	cfg := lnwallet.Config{
		Database:         cdb,
		Notifier:         notifier,
		WalletController: wc,
		Signer:           signer,
		ChainIO:          bio,
		FeeEstimator:     lnwallet.StaticFeeEstimator{FeeRate: 250},
		DefaultConstraints: channeldb.ChannelConstraints{
			DustLimit:        500,
			MaxPendingAmount: lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin) * 100,
			ChanReserve:      100,
			MinHTLC:          400,
			MaxAcceptedHtlcs: 900,
		},
		NetParams: *netParams,
	}

	wallet, err := lnwallet.NewLightningWallet(cfg)
	if err != nil {
		return nil, err
	}

	if err := wallet.Startup(); err != nil {
		return nil, err
	}

	// Load our test wallet with 20 outputs each holding 4BTC.
	if err := loadTestCredits(miningNode, wallet, 20, 4); err != nil {
		return nil, err
	}

	return wallet, nil
}

func testDualFundingReservationWorkflow(miner *rpctest.Harness,
	alice, bob *lnwallet.LightningWallet, t *testing.T) {

	const fundingAmount = btcutil.Amount(5 * 1e8)

	// In this scenario, we'll test a dual funder reservation, with each
	// side putting in 10 BTC.

	// Alice initiates a channel funded with 5 BTC for each side, so 10 BTC
	// total. She also generates 2 BTC in change.
	feePerWeight := btcutil.Amount(alice.Cfg.FeeEstimator.EstimateFeePerWeight(1))
	feePerKw := feePerWeight * 1000
	aliceChanReservation, err := alice.InitChannelReservation(
		fundingAmount*2, fundingAmount, 0, feePerKw,
		bobPub, bobAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}
	aliceChanReservation.SetNumConfsRequired(numReqConfs)
	aliceChanReservation.CommitConstraints(csvDelay, lnwallet.MaxHTLCNumber/2,
		lnwire.NewMSatFromSatoshis(fundingAmount), 10)

	// The channel reservation should now be populated with a multi-sig key
	// from our HD chain, a change output with 3 BTC, and 2 outputs
	// selected of 4 BTC each. Additionally, the rest of the items needed
	// to fulfill a funding contribution should also have been filled in.
	aliceContribution := aliceChanReservation.OurContribution()
	if len(aliceContribution.Inputs) != 2 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should have 2", len(aliceContribution.Inputs))
	}
	assertContributionInitPopulated(t, aliceContribution)

	// Bob does the same, generating his own contribution. He then also
	// receives' Alice's contribution, and consumes that so we can continue
	// the funding process.
	bobChanReservation, err := bob.InitChannelReservation(fundingAmount*2,
		fundingAmount, 0, feePerKw, alicePub, aliceAddr,
		chainHash)
	if err != nil {
		t.Fatalf("bob unable to init channel reservation: %v", err)
	}
	bobChanReservation.CommitConstraints(csvDelay, lnwallet.MaxHTLCNumber/2,
		lnwire.NewMSatFromSatoshis(fundingAmount), 10)
	bobChanReservation.SetNumConfsRequired(numReqConfs)

	assertContributionInitPopulated(t, bobChanReservation.OurContribution())

	err = bobChanReservation.ProcessContribution(aliceContribution)
	if err != nil {
		t.Fatalf("bob unable to process alice's contribution: %v", err)
	}
	assertContributionInitPopulated(t, bobChanReservation.TheirContribution())

	bobContribution := bobChanReservation.OurContribution()

	// Bob then sends over his contribution, which will be consumed by
	// Alice. After this phase, Alice should have all the necessary
	// material required to craft the funding transaction and commitment
	// transactions.
	err = aliceChanReservation.ProcessContribution(bobContribution)
	if err != nil {
		t.Fatalf("alice unable to process bob's contribution: %v", err)
	}
	assertContributionInitPopulated(t, aliceChanReservation.TheirContribution())

	// At this point, all Alice's signatures should be fully populated.
	aliceFundingSigs, aliceCommitSig := aliceChanReservation.OurSignatures()
	if aliceFundingSigs == nil {
		t.Fatalf("alice's funding signatures not populated")
	}
	if aliceCommitSig == nil {
		t.Fatalf("alice's commit signatures not populated")
	}

	// Additionally, Bob's signatures should also be fully populated.
	bobFundingSigs, bobCommitSig := bobChanReservation.OurSignatures()
	if bobFundingSigs == nil {
		t.Fatalf("bob's funding signatures not populated")
	}
	if bobCommitSig == nil {
		t.Fatalf("bob's commit signatures not populated")
	}

	// To concludes, we'll consume first Alice's signatures with Bob, and
	// then the other way around.
	_, err = aliceChanReservation.CompleteReservation(
		bobFundingSigs, bobCommitSig,
	)
	if err != nil {
		t.Fatalf("unable to consume alice's sigs: %v", err)
	}
	_, err = bobChanReservation.CompleteReservation(
		aliceFundingSigs, aliceCommitSig,
	)
	if err != nil {
		t.Fatalf("unable to consume bob's sigs: %v", err)
	}

	// At this point, the funding tx should have been populated.
	fundingTx := aliceChanReservation.FinalFundingTx()
	if fundingTx == nil {
		t.Fatalf("funding transaction never created!")
	}

	// The resulting active channel state should have been persisted to the
	// DB.
	fundingSha := fundingTx.TxHash()
	aliceChannels, err := alice.Cfg.Database.FetchOpenChannels(bobPub)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if !bytes.Equal(aliceChannels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved")
	}
	if aliceChannels[0].ChanType != channeldb.DualFunder {
		t.Fatalf("channel not detected as dual funder")
	}
	bobChannels, err := bob.Cfg.Database.FetchOpenChannels(alicePub)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if !bytes.Equal(bobChannels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved")
	}
	if bobChannels[0].ChanType != channeldb.DualFunder {
		t.Fatalf("channel not detected as dual funder")
	}

	// Mine a single block, the funding transaction should be included
	// within this block.
	blockHashes, err := miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := miner.Node.GetBlock(blockHashes[0])
	if err != nil {
		t.Fatalf("unable to find block: %v", err)
	}
	if len(block.Transactions) != 2 {
		t.Fatalf("funding transaction wasn't mined: %v", err)
	}
	blockTx := block.Transactions[1]
	if blockTx.TxHash() != fundingSha {
		t.Fatalf("incorrect transaction was mined")
	}

	assertReservationDeleted(aliceChanReservation, t)
	assertReservationDeleted(bobChanReservation, t)
}

func testFundingTransactionLockedOutputs(miner *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	// Create a single channel asking for 16 BTC total.
	fundingAmount := btcutil.Amount(8 * 1e8)
	feePerWeight := btcutil.Amount(alice.Cfg.FeeEstimator.EstimateFeePerWeight(1))
	feePerKw := feePerWeight * 1000
	_, err := alice.InitChannelReservation(fundingAmount,
		fundingAmount, 0, feePerKw, bobPub, bobAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation 1: %v", err)
	}

	// Now attempt to reserve funds for another channel, this time
	// requesting 900 BTC. We only have around 64BTC worth of outpoints
	// that aren't locked, so this should fail.
	amt := btcutil.Amount(900 * 1e8)
	failedReservation, err := alice.InitChannelReservation(amt, amt, 0,
		feePerKw, bobPub, bobAddr, chainHash)
	if err == nil {
		t.Fatalf("not error returned, should fail on coin selection")
	}
	if _, ok := err.(*lnwallet.ErrInsufficientFunds); !ok {
		t.Fatalf("error not coinselect error: %v", err)
	}
	if failedReservation != nil {
		t.Fatalf("reservation should be nil")
	}
}

func testFundingCancellationNotEnoughFunds(miner *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	feePerWeight := btcutil.Amount(alice.Cfg.FeeEstimator.EstimateFeePerWeight(1))
	feePerKw := feePerWeight * 1000

	// Create a reservation for 44 BTC.
	fundingAmount := btcutil.Amount(44 * 1e8)
	chanReservation, err := alice.InitChannelReservation(fundingAmount,
		fundingAmount, 0, feePerKw, bobPub, bobAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}

	// Attempt to create another channel with 44 BTC, this should fail.
	_, err = alice.InitChannelReservation(fundingAmount,
		fundingAmount, 0, feePerKw, bobPub, bobAddr, chainHash)
	if _, ok := err.(*lnwallet.ErrInsufficientFunds); !ok {
		t.Fatalf("coin selection succeded should have insufficient funds: %v",
			err)
	}

	// Now cancel that old reservation.
	if err := chanReservation.Cancel(); err != nil {
		t.Fatalf("unable to cancel reservation: %v", err)
	}

	// Those outpoints should no longer be locked.
	lockedOutPoints := alice.LockedOutpoints()
	if len(lockedOutPoints) != 0 {
		t.Fatalf("outpoints still locked")
	}

	// Reservation ID should no longer be tracked.
	numReservations := alice.ActiveReservations()
	if len(alice.ActiveReservations()) != 0 {
		t.Fatalf("should have 0 reservations, instead have %v",
			numReservations)
	}

	// TODO(roasbeef): create method like Balance that ignores locked
	// outpoints, will let us fail early/fast instead of querying and
	// attempting coin selection.

	// Request to fund a new channel should now succeed.
	_, err = alice.InitChannelReservation(fundingAmount, fundingAmount, 0,
		feePerKw, bobPub, bobAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to initialize funding reservation: %v", err)
	}
}

func testCancelNonExistantReservation(miner *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	feeRate := btcutil.Amount(alice.Cfg.FeeEstimator.EstimateFeePerWeight(1))

	// Create our own reservation, give it some ID.
	res := lnwallet.NewChannelReservation(1000, 1000, feeRate, alice,
		22, 10, &testHdSeed)

	// Attempt to cancel this reservation. This should fail, we know
	// nothing of it.
	if err := res.Cancel(); err == nil {
		t.Fatalf("cancelled non-existent reservation")
	}
}

func assertContributionInitPopulated(t *testing.T, c *lnwallet.ChannelContribution) {
	_, _, line, _ := runtime.Caller(1)

	if c.FirstCommitmentPoint == nil {
		t.Fatalf("line #%v: commitment point not fond", line)
	}

	if c.CsvDelay == 0 {
		t.Fatalf("line #%v: csv delay not set", line)
	}

	if c.MultiSigKey == nil {
		t.Fatalf("line #%v: multi-sig key not set", line)
	}
	if c.RevocationBasePoint == nil {
		t.Fatalf("line #%v: revocation key not set", line)
	}
	if c.PaymentBasePoint == nil {
		t.Fatalf("line #%v: payment key not set", line)
	}
	if c.DelayBasePoint == nil {
		t.Fatalf("line #%v: delay key not set", line)
	}

	if c.DustLimit == 0 {
		t.Fatalf("line #%v: dust limit not set", line)
	}
	if c.MaxPendingAmount == 0 {
		t.Fatalf("line #%v: max pending amt not set", line)
	}
	if c.ChanReserve == 0 {
		// TODO(roasbeef): need to follow up and ensure reserve set to
		// fraction
		t.Fatalf("line #%v: chan reserve not set", line)
	}
	if c.MinHTLC == 0 {
		t.Fatalf("line #%v: min htlc not set", line)
	}
	if c.MaxAcceptedHtlcs == 0 {
		t.Fatalf("line #%v: max accepted htlc's not set", line)
	}
}

func testSingleFunderReservationWorkflow(miner *rpctest.Harness,
	alice, bob *lnwallet.LightningWallet, t *testing.T) {

	// For this scenario, Alice will be the channel initiator while bob
	// will act as the responder to the workflow.

	// First, Alice will Initialize a reservation for a channel with 4 BTC
	// funded solely by us. We'll also initially push 1 BTC of the channel
	// towards Bob's side.
	fundingAmt := btcutil.Amount(4 * 1e8)
	pushAmt := lnwire.NewMSatFromSatoshis(btcutil.SatoshiPerBitcoin)
	feePerWeight := btcutil.Amount(alice.Cfg.FeeEstimator.EstimateFeePerWeight(1))
	feePerKw := feePerWeight * 1000
	aliceChanReservation, err := alice.InitChannelReservation(fundingAmt,
		fundingAmt, pushAmt, feePerKw, bobPub, bobAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to init channel reservation: %v", err)
	}
	aliceChanReservation.SetNumConfsRequired(numReqConfs)
	aliceChanReservation.CommitConstraints(csvDelay, lnwallet.MaxHTLCNumber/2,
		lnwire.NewMSatFromSatoshis(fundingAmt), 10)

	// Verify all contribution fields have been set properly.
	aliceContribution := aliceChanReservation.OurContribution()
	if len(aliceContribution.Inputs) < 1 {
		t.Fatalf("outputs for funding tx not properly selected, have %v "+
			"outputs should at least 1", len(aliceContribution.Inputs))
	}
	if len(aliceContribution.ChangeOutputs) != 1 {
		t.Fatalf("coin selection failed, should have one change outputs, "+
			"instead have: %v", len(aliceContribution.ChangeOutputs))
	}
	aliceContribution.CsvDelay = csvDelay
	assertContributionInitPopulated(t, aliceContribution)

	// Next, Bob receives the initial request, generates a corresponding
	// reservation initiation, then consume Alice's contribution.
	bobChanReservation, err := bob.InitChannelReservation(fundingAmt, 0,
		pushAmt, feePerKw, alicePub, aliceAddr, chainHash)
	if err != nil {
		t.Fatalf("unable to create bob reservation: %v", err)
	}
	bobChanReservation.CommitConstraints(csvDelay, lnwallet.MaxHTLCNumber/2,
		lnwire.NewMSatFromSatoshis(fundingAmt), 10)
	bobChanReservation.SetNumConfsRequired(numReqConfs)

	// We'll ensure that Bob's contribution also gets generated properly.
	bobContribution := bobChanReservation.OurContribution()
	bobContribution.CsvDelay = csvDelay
	assertContributionInitPopulated(t, bobContribution)

	// With his contribution generated, he can now process Alice's
	// contribution.
	err = bobChanReservation.ProcessSingleContribution(aliceContribution)
	if err != nil {
		t.Fatalf("bob unable to process alice's contribution: %v", err)
	}
	assertContributionInitPopulated(t, bobChanReservation.TheirContribution())

	// Bob will next send over his contribution to Alice, we simulate this
	// by having Alice immediately process his contribution.
	err = aliceChanReservation.ProcessContribution(bobContribution)
	if err != nil {
		t.Fatalf("alice unable to process bob's contribution")
	}
	assertContributionInitPopulated(t, bobChanReservation.TheirContribution())

	// At this point, Alice should have generated all the signatures
	// required for the funding transaction, as well as Alice's commitment
	// signature to bob.
	aliceRemoteContribution := aliceChanReservation.TheirContribution()
	aliceFundingSigs, aliceCommitSig := aliceChanReservation.OurSignatures()
	if aliceFundingSigs == nil {
		t.Fatalf("funding sigs not found")
	}
	if aliceCommitSig == nil {
		t.Fatalf("commitment sig not found")
	}

	// Additionally, the funding tx and the funding outpoint should have
	// been populated.
	if aliceChanReservation.FinalFundingTx() == nil {
		t.Fatalf("funding transaction never created!")
	}
	if aliceChanReservation.FundingOutpoint() == nil {
		t.Fatalf("funding outpoint never created!")
	}

	// Their funds should also be filled in.
	if len(aliceRemoteContribution.Inputs) != 0 {
		t.Fatalf("bob shouldn't have any inputs, instead has %v",
			len(aliceRemoteContribution.Inputs))
	}
	if len(aliceRemoteContribution.ChangeOutputs) != 0 {
		t.Fatalf("bob shouldn't have any change outputs, instead "+
			"has %v",
			aliceRemoteContribution.ChangeOutputs[0].Value)
	}

	// Next, Alice will send over her signature for Bob's commitment
	// transaction, as well as the funding outpoint.
	fundingPoint := aliceChanReservation.FundingOutpoint()
	_, err = bobChanReservation.CompleteReservationSingle(
		fundingPoint, aliceCommitSig,
	)
	if err != nil {
		t.Fatalf("bob unable to consume single reservation: %v", err)
	}

	// Finally, we'll conclude the reservation process by sending over
	// Bob's commitment signature, which is the final thing Alice needs to
	// be able to safely broadcast the funding transaction.
	_, bobCommitSig := bobChanReservation.OurSignatures()
	if bobCommitSig == nil {
		t.Fatalf("bob failed to generate commitment signature: %v", err)
	}
	_, err = aliceChanReservation.CompleteReservation(
		nil, bobCommitSig,
	)
	if err != nil {
		t.Fatalf("alice unable to complete reservation: %v", err)
	}

	// The resulting active channel state should have been persisted to the
	// DB for both Alice and Bob.
	fundingTx := aliceChanReservation.FinalFundingTx()
	fundingSha := fundingTx.TxHash()
	aliceChannels, err := alice.Cfg.Database.FetchOpenChannels(bobPub)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if len(aliceChannels) != 1 {
		t.Fatalf("alice didn't save channel state: %v", err)
	}
	if !bytes.Equal(aliceChannels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved: %v vs %v",
			hex.EncodeToString(aliceChannels[0].FundingOutpoint.Hash[:]),
			hex.EncodeToString(fundingSha[:]))
	}
	if !aliceChannels[0].IsInitiator {
		t.Fatalf("alice not detected as channel initiator")
	}
	if aliceChannels[0].ChanType != channeldb.SingleFunder {
		t.Fatalf("channel type is incorrect, expected %v instead got %v",
			channeldb.SingleFunder, aliceChannels[0].ChanType)
	}

	bobChannels, err := bob.Cfg.Database.FetchOpenChannels(alicePub)
	if err != nil {
		t.Fatalf("unable to retrieve channel from DB: %v", err)
	}
	if len(bobChannels) != 1 {
		t.Fatalf("bob didn't save channel state: %v", err)
	}
	if !bytes.Equal(bobChannels[0].FundingOutpoint.Hash[:], fundingSha[:]) {
		t.Fatalf("channel state not properly saved: %v vs %v",
			hex.EncodeToString(bobChannels[0].FundingOutpoint.Hash[:]),
			hex.EncodeToString(fundingSha[:]))
	}
	if bobChannels[0].IsInitiator {
		t.Fatalf("bob not detected as channel responder")
	}
	if bobChannels[0].ChanType != channeldb.SingleFunder {
		t.Fatalf("channel type is incorrect, expected %v instead got %v",
			channeldb.SingleFunder, bobChannels[0].ChanType)
	}

	// Mine a single block, the funding transaction should be included
	// within this block.
	blockHashes, err := miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}
	block, err := miner.Node.GetBlock(blockHashes[0])
	if err != nil {
		t.Fatalf("unable to find block: %v", err)
	}
	if len(block.Transactions) != 2 {
		t.Fatalf("funding transaction wasn't mined: %v", err)
	}
	blockTx := block.Transactions[1]
	if blockTx.TxHash() != fundingSha {
		t.Fatalf("incorrect transaction was mined")
	}

	assertReservationDeleted(aliceChanReservation, t)
	assertReservationDeleted(bobChanReservation, t)
}

func testListTransactionDetails(miner *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	// Create 5 new outputs spendable by the wallet.
	const numTxns = 5
	const outputAmt = btcutil.SatoshiPerBitcoin
	txids := make(map[chainhash.Hash]struct{})
	for i := 0; i < numTxns; i++ {
		addr, err := alice.NewAddress(lnwallet.WitnessPubKey, false)
		if err != nil {
			t.Fatalf("unable to create new address: %v", err)
		}
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			t.Fatalf("unable to create output script: %v", err)
		}

		output := &wire.TxOut{
			Value:    outputAmt,
			PkScript: script,
		}
		txid, err := miner.SendOutputs([]*wire.TxOut{output}, 10)
		if err != nil {
			t.Fatalf("unable to send coinbase: %v", err)
		}
		txids[*txid] = struct{}{}
	}

	// Generate 10 blocks to mine all the transactions created above.
	const numBlocksMined = 10
	blocks, err := miner.Node.Generate(numBlocksMined)
	if err != nil {
		t.Fatalf("unable to mine blocks: %v", err)
	}

	// Next, fetch all the current transaction details.
	// TODO(roasbeef): use ntfn client here instead?
	time.Sleep(time.Second * 2)
	txDetails, err := alice.ListTransactionDetails()
	if err != nil {
		t.Fatalf("unable to fetch tx details: %v", err)
	}

	// Each of the transactions created above should be found with the
	// proper details populated.
	for _, txDetail := range txDetails {
		if _, ok := txids[txDetail.Hash]; !ok {
			continue
		}

		if txDetail.NumConfirmations != numBlocksMined {
			t.Fatalf("num confs incorrect, got %v expected %v",
				txDetail.NumConfirmations, numBlocksMined)
		}
		if txDetail.Value != outputAmt {
			t.Fatalf("tx value incorrect, got %v expected %v",
				txDetail.Value, outputAmt)
		}
		if !bytes.Equal(txDetail.BlockHash[:], blocks[0][:]) {
			t.Fatalf("block hash mismatch, got %v expected %v",
				txDetail.BlockHash, blocks[0])
		}

		delete(txids, txDetail.Hash)
	}
	if len(txids) != 0 {
		t.Fatalf("all transactions not found in details!")
	}

	// Next create a transaction paying to an output which isn't under the
	// wallet's control.
	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_0)
	outputScript, err := b.Script()
	if err != nil {
		t.Fatalf("unable to make output script: %v", err)
	}
	burnOutput := wire.NewTxOut(outputAmt, outputScript)
	burnTXID, err := alice.SendOutputs([]*wire.TxOut{burnOutput})
	if err != nil {
		t.Fatalf("unable to create burn tx: %v", err)
	}
	burnBlock, err := miner.Node.Generate(1)
	if err != nil {
		t.Fatalf("unable to mine block: %v", err)
	}

	// Fetch the transaction details again, the new transaction should be
	// shown as debiting from the wallet's balance.
	time.Sleep(time.Second * 2)
	txDetails, err = alice.ListTransactionDetails()
	if err != nil {
		t.Fatalf("unable to fetch tx details: %v", err)
	}
	var burnTxFound bool
	for _, txDetail := range txDetails {
		if !bytes.Equal(txDetail.Hash[:], burnTXID[:]) {
			continue
		}

		burnTxFound = true
		if txDetail.NumConfirmations != 1 {
			t.Fatalf("num confs incorrect, got %v expected %v",
				txDetail.NumConfirmations, 1)
		}
		if txDetail.Value >= -outputAmt {
			t.Fatalf("tx value incorrect, got %v expected %v",
				txDetail.Value, -outputAmt)
		}
		if !bytes.Equal(txDetail.BlockHash[:], burnBlock[0][:]) {
			t.Fatalf("block hash mismatch, got %v expected %v",
				txDetail.BlockHash, burnBlock[0])
		}
	}
	if !burnTxFound {
		t.Fatal("tx burning btc not found")
	}
}

func testTransactionSubscriptions(miner *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	// First, check to see if this wallet meets the TransactionNotifier
	// interface, if not then we'll skip this test for this particular
	// implementation of the WalletController.
	txClient, err := alice.SubscribeTransactions()
	if err != nil {
		t.Fatalf("unable to generate tx subscription: %v", err)
	}
	defer txClient.Cancel()

	const (
		outputAmt = btcutil.SatoshiPerBitcoin
		numTxns   = 3
	)
	unconfirmedNtfns := make(chan struct{})
	go func() {
		for i := 0; i < numTxns; i++ {
			txDetail := <-txClient.UnconfirmedTransactions()
			if txDetail.NumConfirmations != 0 {
				t.Fatalf("incorrect number of confs, expected %v got %v",
					0, txDetail.NumConfirmations)
			}
			if txDetail.Value != outputAmt {
				t.Fatalf("incorrect output amt, expected %v got %v",
					outputAmt, txDetail.Value)
			}
			if txDetail.BlockHash != nil {
				t.Fatalf("block hash should be nil, is instead %v",
					txDetail.BlockHash)
			}
		}

		close(unconfirmedNtfns)
	}()

	// Next, fetch a fresh address from the wallet, create 3 new outputs
	// with the pkScript.
	for i := 0; i < numTxns; i++ {
		addr, err := alice.NewAddress(lnwallet.WitnessPubKey, false)
		if err != nil {
			t.Fatalf("unable to create new address: %v", err)
		}
		script, err := txscript.PayToAddrScript(addr)
		if err != nil {
			t.Fatalf("unable to create output script: %v", err)
		}

		output := &wire.TxOut{
			Value:    outputAmt,
			PkScript: script,
		}
		if _, err := miner.SendOutputs([]*wire.TxOut{output}, 10); err != nil {
			t.Fatalf("unable to send coinbase: %v", err)
		}
	}

	// We should receive a notification for all three transactions
	// generated above.
	select {
	case <-time.After(time.Second * 5):
		t.Fatalf("transactions not received after 3 seconds")
	case <-unconfirmedNtfns: // Fall through on successs
	}

	confirmedNtfns := make(chan struct{})
	go func() {
		for i := 0; i < numTxns; i++ {
			txDetail := <-txClient.ConfirmedTransactions()
			if txDetail.NumConfirmations != 1 {
				t.Fatalf("incorrect number of confs, expected %v got %v",
					1, txDetail.NumConfirmations)
			}
			if txDetail.Value != outputAmt {
				t.Fatalf("incorrect output amt, expected %v got %v",
					outputAmt, txDetail.Value)
			}
		}
		close(confirmedNtfns)
	}()

	// Next mine a single block, all the transactions generated above
	// should be included.
	if _, err := miner.Node.Generate(1); err != nil {
		t.Fatalf("unable to generate block: %v", err)
	}

	// We should receive a notification for all three transactions
	// since they should be mined in the next block.
	select {
	case <-time.After(time.Second * 5):
		t.Fatalf("transactions not received after 3 seconds")
	case <-confirmedNtfns: // Fall through on success
	}
}

func testSignOutputUsingTweaks(r *rpctest.Harness,
	alice, _ *lnwallet.LightningWallet, t *testing.T) {

	// We'd like to test the ability of the wallet's Signer implementation
	// to be able to sign with a private key derived from tweaking the
	// specific public key. This scenario exercises the case when the
	// wallet needs to sign for a sweep of a revoked output, or just claim
	// any output that pays to a tweaked key.

	// First, generate a new public key under the control of the wallet,
	// then generate a revocation key using it.
	pubKey, err := alice.NewRawKey()
	if err != nil {
		t.Fatalf("unable to obtain public key: %v", err)
	}

	// As we'd like to test both single tweak, and double tweak spends,
	// we'll generate a commitment pre-image, then derive a revocation key
	// and single tweak from that.
	commitPreimage := bytes.Repeat([]byte{2}, 32)
	commitSecret, commitPoint := btcec.PrivKeyFromBytes(btcec.S256(),
		commitPreimage)

	revocationKey := lnwallet.DeriveRevocationPubkey(pubKey, commitPoint)
	commitTweak := lnwallet.SingleTweakBytes(commitPoint, pubKey)

	tweakedPub := lnwallet.TweakPubKey(pubKey, commitPoint)

	// As we'd like to test both single and double tweaks, we'll repeat
	// the same set up twice. The first will use a regular single tweak,
	// and the second will use a double tweak.
	baseKey := pubKey
	for i := 0; i < 2; i++ {
		var tweakedKey *btcec.PublicKey
		if i == 0 {
			tweakedKey = tweakedPub
		} else {
			tweakedKey = revocationKey
		}

		// Using the given key for the current iteration, we'll
		// generate a regular p2wkh from that.
		pubkeyHash := btcutil.Hash160(tweakedKey.SerializeCompressed())
		keyAddr, err := btcutil.NewAddressWitnessPubKeyHash(pubkeyHash,
			&chaincfg.SimNetParams)
		if err != nil {
			t.Fatalf("unable to create addr: %v", err)
		}
		keyScript, err := txscript.PayToAddrScript(keyAddr)
		if err != nil {
			t.Fatalf("unable to generate script: %v", err)
		}

		// With the script fully assembled, instruct the wallet to fund
		// the output with a newly created transaction.
		newOutput := &wire.TxOut{
			Value:    btcutil.SatoshiPerBitcoin,
			PkScript: keyScript,
		}
		txid, err := alice.SendOutputs([]*wire.TxOut{newOutput})
		if err != nil {
			t.Fatalf("unable to create output: %v", err)
		}

		// Query for the transaction generated above so we can located
		// the index of our output.
		tx, err := r.Node.GetRawTransaction(txid)
		if err != nil {
			t.Fatalf("unable to query for tx: %v", err)
		}
		var outputIndex uint32
		if bytes.Equal(tx.MsgTx().TxOut[0].PkScript, keyScript) {
			outputIndex = 0
		} else {
			outputIndex = 1
		}

		// With the index located, we can create a transaction spending
		// the referenced output.
		sweepTx := wire.NewMsgTx(2)
		sweepTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  tx.MsgTx().TxHash(),
				Index: outputIndex,
			},
		})
		sweepTx.AddTxOut(&wire.TxOut{
			Value:    1000,
			PkScript: keyScript,
		})

		// Now we can populate the sign descriptor which we'll use to
		// generate the signature. Within the descriptor we set the
		// private tweak value as the key in the script is derived
		// based on this tweak value and the key we originally
		// generated above.
		signDesc := &lnwallet.SignDescriptor{
			PubKey:        baseKey,
			WitnessScript: keyScript,
			Output:        newOutput,
			HashType:      txscript.SigHashAll,
			SigHashes:     txscript.NewTxSigHashes(sweepTx),
			InputIndex:    0,
		}

		// If this is the first, loop, we'll use the generated single
		// tweak, otherwise, we'll use the double tweak.
		if i == 0 {
			signDesc.SingleTweak = commitTweak
		} else {
			signDesc.DoubleTweak = commitSecret
		}

		// With the descriptor created, we use it to generate a
		// signature, then manually create a valid witness stack we'll
		// use for signing.
		spendSig, err := alice.Cfg.Signer.SignOutputRaw(sweepTx, signDesc)
		if err != nil {
			t.Fatalf("unable to generate signature: %v", err)
		}
		witness := make([][]byte, 2)
		witness[0] = append(spendSig, byte(txscript.SigHashAll))
		witness[1] = tweakedKey.SerializeCompressed()
		sweepTx.TxIn[0].Witness = witness

		// Finally, attempt to validate the completed transaction. This
		// should succeed if the wallet was able to properly generate
		// the proper private key.
		vm, err := txscript.NewEngine(keyScript,
			sweepTx, 0, txscript.StandardVerifyFlags, nil,
			nil, int64(btcutil.SatoshiPerBitcoin))
		if err != nil {
			t.Fatalf("unable to create engine: %v", err)
		}
		if err := vm.Execute(); err != nil {
			t.Fatalf("spend #%v is invalid: %v", i, err)
		}
	}
}

type walletTestCase struct {
	name string
	test func(miner *rpctest.Harness, alice, bob *lnwallet.LightningWallet,
		test *testing.T)
}

var walletTests = []walletTestCase{
	{
		name: "single funding workflow",
		test: testSingleFunderReservationWorkflow,
	},
	{
		name: "dual funder workflow",
		test: testDualFundingReservationWorkflow,
	},
	{
		name: "output locking",
		test: testFundingTransactionLockedOutputs,
	},
	{
		name: "reservation insufficient funds",
		test: testFundingCancellationNotEnoughFunds,
	},
	{
		name: "transaction subscriptions",
		test: testTransactionSubscriptions,
	},
	{
		name: "transaction details",
		test: testListTransactionDetails,
	},
	{
		name: "signed with tweaked pubkeys",
		test: testSignOutputUsingTweaks,
	},
	{
		name: "test cancel non-existent reservation",
		test: testCancelNonExistantReservation,
	},
}

func clearWalletStates(a, b *lnwallet.LightningWallet) error {
	a.ResetReservations()
	b.ResetReservations()

	if err := a.Cfg.Database.Wipe(); err != nil {
		return err
	}

	return b.Cfg.Database.Wipe()
}

// TestInterfaces tests all registered interfaces with a unified set of tests
// which excersie each of the required methods found within the WalletController
// interface.
//
// NOTE: In the future, when additional implementations of the WalletController
// interface have been implemented, in order to ensure the new concrete
// implementation is automatically tested, two steps must be undertaken. First,
// one needs add a "non-captured" (_) import from the new sub-package. This
// import should trigger an init() method within the package which registers
// the interface. Second, an additional case in the switch within the main loop
// below needs to be added which properly initializes the interface.
//
// TODO(roasbeef): purge bobNode in favor of dual lnwallet's
func TestLightningWallet(t *testing.T) {
	t.Parallel()

	// Initialize the harness around a btcd node which will serve as our
	// dedicated miner to generate blocks, cause re-orgs, etc. We'll set
	// up this node with a chain length of 125, so we have plentyyy of BTC
	// to play around with.
	miningNode, err := rpctest.New(netParams, nil, nil)
	if err != nil {
		t.Fatalf("unable to create mining node: %v", err)
	}
	defer miningNode.TearDown()
	if err := miningNode.SetUp(true, 25); err != nil {
		t.Fatalf("unable to set up mining node: %v", err)
	}

	// Next mine enough blocks in order for segwit and the CSV package
	// soft-fork to activate on SimNet.
	numBlocks := netParams.MinerConfirmationWindow * 2
	if _, err := miningNode.Node.Generate(numBlocks); err != nil {
		t.Fatalf("unable to generate blocks: %v", err)
	}

	rpcConfig := miningNode.RPCConfig()

	chainNotifier, err := btcdnotify.New(&rpcConfig)
	if err != nil {
		t.Fatalf("unable to create notifier: %v", err)
	}
	if err := chainNotifier.Start(); err != nil {
		t.Fatalf("unable to start notifier: %v", err)
	}

	var (
		bio lnwallet.BlockChainIO

		aliceSigner lnwallet.Signer
		bobSigner   lnwallet.Signer

		aliceWalletController lnwallet.WalletController
		bobWalletController   lnwallet.WalletController
	)
	for _, walletDriver := range lnwallet.RegisteredWallets() {
		tempTestDirAlice, err := ioutil.TempDir("", "lnwallet")
		if err != nil {
			t.Fatalf("unable to create temp directory: %v", err)
		}
		defer os.RemoveAll(tempTestDirAlice)

		tempTestDirBob, err := ioutil.TempDir("", "lnwallet")
		if err != nil {
			t.Fatalf("unable to create temp directory: %v", err)
		}
		defer os.RemoveAll(tempTestDirBob)

		walletType := walletDriver.WalletType
		switch walletType {
		case "btcwallet":
			aliceChainRpc, err := chain.NewRPCClient(netParams,
				rpcConfig.Host, rpcConfig.User, rpcConfig.Pass,
				rpcConfig.Certificates, false, 20)
			if err != nil {
				t.Fatalf("unable to make chain rpc: %v", err)
			}
			aliceWalletConfig := &btcwallet.Config{
				PrivatePass:  []byte("alice-pass"),
				HdSeed:       aliceHDSeed[:],
				DataDir:      tempTestDirAlice,
				NetParams:    netParams,
				ChainSource:  aliceChainRpc,
				FeeEstimator: lnwallet.StaticFeeEstimator{FeeRate: 250},
			}
			aliceWalletController, err = walletDriver.New(aliceWalletConfig)
			if err != nil {
				t.Fatalf("unable to create btcwallet: %v", err)
			}
			aliceSigner = aliceWalletController.(*btcwallet.BtcWallet)

			bobChainRpc, err := chain.NewRPCClient(netParams,
				rpcConfig.Host, rpcConfig.User, rpcConfig.Pass,
				rpcConfig.Certificates, false, 20)
			if err != nil {
				t.Fatalf("unable to make chain rpc: %v", err)
			}
			bobWalletConfig := &btcwallet.Config{
				PrivatePass:  []byte("bob-pass"),
				HdSeed:       bobHDSeed[:],
				DataDir:      tempTestDirBob,
				NetParams:    netParams,
				ChainSource:  bobChainRpc,
				FeeEstimator: lnwallet.StaticFeeEstimator{FeeRate: 250},
			}
			bobWalletController, err = walletDriver.New(bobWalletConfig)
			if err != nil {
				t.Fatalf("unable to create btcwallet: %v", err)
			}
			bobSigner = bobWalletController.(*btcwallet.BtcWallet)
			bio = bobWalletController.(*btcwallet.BtcWallet)
		default:
			// TODO(roasbeef): add neutrino case
			t.Fatalf("unknown wallet driver: %v", walletType)
		}

		// Funding via 20 outputs with 4BTC each.
		alice, err := createTestWallet(tempTestDirAlice, miningNode,
			netParams, chainNotifier, aliceWalletController,
			aliceSigner, bio)
		if err != nil {
			t.Fatalf("unable to create test ln wallet: %v", err)
		}
		defer alice.Shutdown()

		bob, err := createTestWallet(tempTestDirBob, miningNode,
			netParams, chainNotifier, bobWalletController,
			bobSigner, bio)
		if err != nil {
			t.Fatalf("unable to create test ln wallet: %v", err)
		}
		defer bob.Shutdown()

		// Both wallets should now have 80BTC available for spending.
		assertProperBalance(t, alice, 1, 80)
		assertProperBalance(t, bob, 1, 80)

		// Execute every test, clearing possibly mutated wallet state
		// after each step.
		for _, walletTest := range walletTests {
			testName := fmt.Sprintf("%v:%v", walletType,
				walletTest.name)
			success := t.Run(testName, func(t *testing.T) {
				walletTest.test(miningNode, alice, bob, t)
			})
			if !success {
				break
			}

			// TODO(roasbeef): possible reset mining node's
			// chainstate to initial level, cleanly wipe buckets
			if err := clearWalletStates(alice, bob); err != nil &&
				err != bolt.ErrBucketNotFound {
				t.Fatalf("unable to wipe wallet state: %v", err)
			}
		}
	}
}
