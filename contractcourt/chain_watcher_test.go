package contractcourt

import (
	"bytes"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

type mockNotifier struct {
	spendChan chan *chainntnfs.SpendDetail
	epochChan chan *chainntnfs.BlockEpoch
	confChan  chan *chainntnfs.TxConfirmation
}

func (m *mockNotifier) RegisterConfirmationsNtfn(txid *chainhash.Hash, _ []byte, numConfs,
	heightHint uint32) (*chainntnfs.ConfirmationEvent, error) {
	return &chainntnfs.ConfirmationEvent{
		Confirmed: m.confChan,
	}, nil
}

func (m *mockNotifier) RegisterBlockEpochNtfn(
	bestBlock *chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error) {

	return &chainntnfs.BlockEpochEvent{
		Epochs: m.epochChan,
		Cancel: func() {},
	}, nil
}

func (m *mockNotifier) Start() error {
	return nil
}

func (m *mockNotifier) Stop() error {
	return nil
}
func (m *mockNotifier) RegisterSpendNtfn(outpoint *wire.OutPoint, _ []byte,
	heightHint uint32) (*chainntnfs.SpendEvent, error) {

	return &chainntnfs.SpendEvent{
		Spend:  m.spendChan,
		Cancel: func() {},
	}, nil
}

// TestChainWatcherRemoteUnilateralClose tests that the chain watcher is able
// to properly detect a normal unilateral close by the remote node using their
// lowest commitment.
func TestChainWatcherRemoteUnilateralClose(t *testing.T) {
	t.Parallel()

	// First, we'll create two channels which already have established a
	// commitment contract between themselves.
	aliceChannel, bobChannel, cleanUp, err := lnwallet.CreateTestChannels()
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// With the channels created, we'll now create a chain watcher instance
	// which will be watching for any closes of Alice's channel.
	aliceNotifier := &mockNotifier{
		spendChan: make(chan *chainntnfs.SpendDetail),
	}
	aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState: aliceChannel.State(),
		notifier:  aliceNotifier,
		signer:    aliceChannel.Signer,
	})
	if err != nil {
		t.Fatalf("unable to create chain watcher: %v", err)
	}
	err = aliceChainWatcher.Start()
	if err != nil {
		t.Fatalf("unable to start chain watcher: %v", err)
	}
	defer aliceChainWatcher.Stop()

	// We'll request a new channel event subscription from Alice's chain
	// watcher.
	chanEvents := aliceChainWatcher.SubscribeChannelEvents()

	// If we simulate an immediate broadcast of the current commitment by
	// Bob, then the chain watcher should detect this case.
	bobCommit := bobChannel.State().LocalCommitment.CommitTx
	bobTxHash := bobCommit.TxHash()
	bobSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &bobTxHash,
		SpendingTx:    bobCommit,
	}
	aliceNotifier.spendChan <- bobSpend

	// We should get a new spend event over the remote unilateral close
	// event channel.
	var uniClose *lnwallet.UnilateralCloseSummary
	select {
	case uniClose = <-chanEvents.RemoteUnilateralClosure:
	case <-time.After(time.Second * 15):
		t.Fatalf("didn't receive unilateral close event")
	}

	// The unilateral close should have properly located Alice's output in
	// the commitment transaction.
	if uniClose.CommitResolution == nil {
		t.Fatalf("unable to find alice's commit resolution")
	}
}

// TestChainWatcherRemoteUnilateralClosePendingCommit tests that the chain
// watcher is able to properly detect a unilateral close wherein the remote
// node broadcasts their newly received commitment, without first revoking the
// old one.
func TestChainWatcherRemoteUnilateralClosePendingCommit(t *testing.T) {
	t.Parallel()

	// First, we'll create two channels which already have established a
	// commitment contract between themselves.
	aliceChannel, bobChannel, cleanUp, err := lnwallet.CreateTestChannels()
	if err != nil {
		t.Fatalf("unable to create test channels: %v", err)
	}
	defer cleanUp()

	// With the channels created, we'll now create a chain watcher instance
	// which will be watching for any closes of Alice's channel.
	aliceNotifier := &mockNotifier{
		spendChan: make(chan *chainntnfs.SpendDetail),
	}
	aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState: aliceChannel.State(),
		notifier:  aliceNotifier,
		signer:    aliceChannel.Signer,
	})
	if err != nil {
		t.Fatalf("unable to create chain watcher: %v", err)
	}
	if err := aliceChainWatcher.Start(); err != nil {
		t.Fatalf("unable to start chain watcher: %v", err)
	}
	defer aliceChainWatcher.Stop()

	// We'll request a new channel event subscription from Alice's chain
	// watcher.
	chanEvents := aliceChainWatcher.SubscribeChannelEvents()

	// Next, we'll create a fake HTLC just so we can advance Alice's
	// channel state to a new pending commitment on her remote commit chain
	// for Bob.
	htlcAmount := lnwire.NewMSatFromSatoshis(20000)
	preimage := bytes.Repeat([]byte{byte(1)}, 32)
	paymentHash := sha256.Sum256(preimage)
	var returnPreimage [32]byte
	copy(returnPreimage[:], preimage)
	htlc := &lnwire.UpdateAddHTLC{
		ID:          uint64(0),
		PaymentHash: paymentHash,
		Amount:      htlcAmount,
		Expiry:      uint32(5),
	}

	if _, err := aliceChannel.AddHTLC(htlc, nil); err != nil {
		t.Fatalf("alice unable to add htlc: %v", err)
	}
	if _, err := bobChannel.ReceiveHTLC(htlc); err != nil {
		t.Fatalf("bob unable to recv add htlc: %v", err)
	}

	// With the HTLC added, we'll now manually initiate a state transition
	// from Alice to Bob.
	_, _, err = aliceChannel.SignNextCommitment()
	if err != nil {
		t.Fatal(err)
	}

	// At this point, we'll now Bob broadcasting this new pending unrevoked
	// commitment.
	bobPendingCommit, err := aliceChannel.State().RemoteCommitChainTip()
	if err != nil {
		t.Fatal(err)
	}

	// We'll craft a fake spend notification with Bob's actual commitment.
	// The chain watcher should be able to detect that this is a pending
	// commit broadcast based on the state hints in the commitment.
	bobCommit := bobPendingCommit.Commitment.CommitTx
	bobTxHash := bobCommit.TxHash()
	bobSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &bobTxHash,
		SpendingTx:    bobCommit,
	}
	aliceNotifier.spendChan <- bobSpend

	// We should get a new spend event over the remote unilateral close
	// event channel.
	var uniClose *lnwallet.UnilateralCloseSummary
	select {
	case uniClose = <-chanEvents.RemoteUnilateralClosure:
	case <-time.After(time.Second * 15):
		t.Fatalf("didn't receive unilateral close event")
	}

	// The unilateral close should have properly located Alice's output in
	// the commitment transaction.
	if uniClose.CommitResolution == nil {
		t.Fatalf("unable to find alice's commit resolution")
	}
}
