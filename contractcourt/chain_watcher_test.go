package contractcourt

import (
	"bytes"
	"crypto/sha256"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
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
		chanState:           aliceChannel.State(),
		notifier:            aliceNotifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
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

func addFakeHTLC(t *testing.T, htlcAmount lnwire.MilliSatoshi, id uint64,
	aliceChannel, bobChannel *lnwallet.LightningChannel) {

	preimage := bytes.Repeat([]byte{byte(id)}, 32)
	paymentHash := sha256.Sum256(preimage)
	var returnPreimage [32]byte
	copy(returnPreimage[:], preimage)
	htlc := &lnwire.UpdateAddHTLC{
		ID:          uint64(id),
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
		chanState:           aliceChannel.State(),
		notifier:            aliceNotifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
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
	addFakeHTLC(t, htlcAmount, 0, aliceChannel, bobChannel)

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

// dlpTestCase is a speical struct that we'll use to generate randomized test
// cases for the main TestChainWatcherDataLossProtect test. This struct has a
// special Generate method that will generate a random state number, and a
// broadcast state number which is greater than that state number.
type dlpTestCase struct {
	BroadcastStateNum uint8
	NumUpdates        uint8
}

// TestChainWatcherDataLossProtect tests that if we've lost data (and are
// behind the remote node), then we'll properly detect this case and dispatch a
// remote force close using the obtained data loss commitment point.
func TestChainWatcherDataLossProtect(t *testing.T) {
	t.Parallel()

	// dlpScenario is our primary quick check testing function for this
	// test as whole. It ensures that if the remote party broadcasts a
	// commitment that is beyond our best known commitment for them, and
	// they don't have a pending commitment (one we sent but which hasn't
	// been revoked), then we'll properly detect this case, and execute the
	// DLP protocol on our end.
	//
	// broadcastStateNum is the number that we'll trick Alice into thinking
	// was broadcast, while numUpdates is the actual number of updates
	// we'll execute. Both of these will be random 8-bit values generated
	// by testing/quick.
	dlpScenario := func(testCase dlpTestCase) bool {
		// First, we'll create two channels which already have
		// established a commitment contract between themselves.
		aliceChannel, bobChannel, cleanUp, err := lnwallet.CreateTestChannels()
		if err != nil {
			t.Fatalf("unable to create test channels: %v", err)
		}
		defer cleanUp()

		// With the channels created, we'll now create a chain watcher
		// instance which will be watching for any closes of Alice's
		// channel.
		aliceNotifier := &mockNotifier{
			spendChan: make(chan *chainntnfs.SpendDetail),
		}
		aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
			chanState: aliceChannel.State(),
			notifier:  aliceNotifier,
			signer:    aliceChannel.Signer,
			extractStateNumHint: func(*wire.MsgTx,
				[lnwallet.StateHintSize]byte) uint64 {

				// We'll return the "fake" broadcast commitment
				// number so we can simulate broadcast of an
				// arbitrary state.
				return uint64(testCase.BroadcastStateNum)
			},
		})
		if err != nil {
			t.Fatalf("unable to create chain watcher: %v", err)
		}
		if err := aliceChainWatcher.Start(); err != nil {
			t.Fatalf("unable to start chain watcher: %v", err)
		}
		defer aliceChainWatcher.Stop()

		// Based on the number of random updates for this state, make a
		// new HTLC to add to the commitment, and then lock in a state
		// transition.
		const htlcAmt = 1000
		for i := 0; i < int(testCase.NumUpdates); i++ {
			addFakeHTLC(
				t, 1000, uint64(i), aliceChannel, bobChannel,
			)

			err := lnwallet.ForceStateTransition(
				aliceChannel, bobChannel,
			)
			if err != nil {
				t.Errorf("unable to trigger state "+
					"transition: %v", err)
				return false
			}
		}

		// We'll request a new channel event subscription from Alice's
		// chain watcher so we can be notified of our fake close below.
		chanEvents := aliceChainWatcher.SubscribeChannelEvents()

		// Otherwise, we'll feed in this new state number as a response
		// to the query, and insert the expected DLP commit point.
		dlpPoint := aliceChannel.State().RemoteCurrentRevocation
		err = aliceChannel.State().MarkDataLoss(dlpPoint)
		if err != nil {
			t.Errorf("unable to insert dlp point: %v", err)
			return false
		}

		// Now we'll trigger the channel close event to trigger the
		// scenario.
		bobCommit := bobChannel.State().LocalCommitment.CommitTx
		bobTxHash := bobCommit.TxHash()
		bobSpend := &chainntnfs.SpendDetail{
			SpenderTxHash: &bobTxHash,
			SpendingTx:    bobCommit,
		}
		aliceNotifier.spendChan <- bobSpend

		// We should get a new uni close resolution that indicates we
		// processed the DLP scenario.
		var uniClose *lnwallet.UnilateralCloseSummary
		select {
		case uniClose = <-chanEvents.RemoteUnilateralClosure:
			// If we processed this as a DLP case, then the remote
			// party's commitment should be blank, as we don't have
			// this up to date state.
			blankCommit := channeldb.ChannelCommitment{}
			if uniClose.RemoteCommit.FeePerKw != blankCommit.FeePerKw {
				t.Errorf("DLP path not executed")
				return false
			}

			// The resolution should have also read the DLP point
			// we stored above, and used that to derive their sweep
			// key for this output.
			sweepTweak := input.SingleTweakBytes(
				dlpPoint,
				aliceChannel.State().LocalChanCfg.PaymentBasePoint.PubKey,
			)
			commitResolution := uniClose.CommitResolution
			resolutionTweak := commitResolution.SelfOutputSignDesc.SingleTweak
			if !bytes.Equal(sweepTweak, resolutionTweak) {
				t.Errorf("sweep key mismatch: expected %x got %x",
					sweepTweak, resolutionTweak)
				return false
			}

			return true

		case <-time.After(time.Second * 5):
			t.Errorf("didn't receive unilateral close event")
			return false
		}
	}

	// For our first scenario, we'll ensure that if we're on state 1, and
	// the remote party broadcasts state 2 and we don't have a pending
	// commit for them, then we'll properly detect this as a DLP scenario.
	if !dlpScenario(dlpTestCase{
		BroadcastStateNum: 2,
		NumUpdates:        1,
	}) {
		t.Fatalf("DLP test case failed at state 1!")
	}

	// For the remainder of the tests, we'll perform 10 iterations with
	// random values. We limit this number as set up of each test can take
	// time, and also it doing up to 255 state transitions may cause the
	// test to hang for a long time.
	//
	// TODO(roasbeef): speed up execution
	err := quick.Check(dlpScenario, &quick.Config{
		MaxCount: 10,
		Values: func(v []reflect.Value, rand *rand.Rand) {
			// stateNum will be the random number of state updates
			// we execute during the scenario.
			stateNum := uint8(rand.Int31())

			// From this state number, we'll draw a random number
			// between the state and 255, ensuring that it' at
			// least one state beyond the target stateNum.
			broadcastRange := rand.Int31n(int32(math.MaxUint8 - stateNum))
			broadcastNum := uint8(stateNum + 1 + uint8(broadcastRange))

			testCase := dlpTestCase{
				BroadcastStateNum: broadcastNum,
				NumUpdates:        stateNum,
			}
			v[0] = reflect.ValueOf(testCase)
		},
	})
	if err != nil {
		t.Fatalf("DLP test case failed: %v", err)
	}
}
