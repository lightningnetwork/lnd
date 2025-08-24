package contractcourt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	lnmock "github.com/lightningnetwork/lnd/lntest/mock"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// TestChainWatcherRemoteUnilateralClose tests that the chain watcher is able
// to properly detect a normal unilateral close by the remote node using their
// lowest commitment.
func TestChainWatcherRemoteUnilateralClose(t *testing.T) {
	t.Parallel()

	// First, we'll create two channels which already have established a
	// commitment contract between themselves.
	aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err, "unable to create test channels")

	// With the channels created, we'll now create a chain watcher instance
	// which will be watching for any closes of Alice's channel.
	aliceNotifier := &lnmock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail, 1),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState:           aliceChannel.State(),
		notifier:            aliceNotifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
	})
	require.NoError(t, err, "unable to create chain watcher")
	err = aliceChainWatcher.Start()
	require.NoError(t, err, "unable to start chain watcher")
	defer aliceChainWatcher.Stop()

	// Create a mock blockbeat and send it to Alice's BlockbeatChan.
	mockBeat := &chainio.MockBlockbeat{}

	// Mock the logger. We don't care how many times it's called as it's
	// not critical.
	mockBeat.On("logger").Return(log)

	// Mock a fake block height - this is called based on the debuglevel.
	mockBeat.On("Height").Return(int32(1)).Maybe()

	// Mock `NotifyBlockProcessed` to be call once.
	mockBeat.On("NotifyBlockProcessed",
		nil, aliceChainWatcher.quit).Return().Once()

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

	// Here we mock the behavior of a restart.
	select {
	case aliceNotifier.SpendChan <- bobSpend:
	case <-time.After(1 * time.Second):
		t.Fatalf("unable to send spend details")
	}

	select {
	case aliceChainWatcher.BlockbeatChan <- mockBeat:
	case <-time.After(time.Second * 1):
		t.Fatalf("unable to send blockbeat")
	}

	// We should get a new spend event over the remote unilateral close
	// event channel.
	var uniClose *RemoteUnilateralCloseInfo
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
	aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
		t, channeldb.SingleFunderTweaklessBit,
	)
	require.NoError(t, err, "unable to create test channels")

	// With the channels created, we'll now create a chain watcher instance
	// which will be watching for any closes of Alice's channel.
	aliceNotifier := &lnmock.ChainNotifier{
		SpendChan: make(chan *chainntnfs.SpendDetail),
		EpochChan: make(chan *chainntnfs.BlockEpoch),
		ConfChan:  make(chan *chainntnfs.TxConfirmation),
	}
	aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
		chanState:           aliceChannel.State(),
		notifier:            aliceNotifier,
		signer:              aliceChannel.Signer,
		extractStateNumHint: lnwallet.GetStateNumHint,
	})
	require.NoError(t, err, "unable to create chain watcher")
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
	testQuit, testQuitFunc := context.WithCancel(t.Context())
	t.Cleanup(testQuitFunc)
	_, err = aliceChannel.SignNextCommitment(testQuit)
	require.NoError(t, err)

	// At this point, we'll now Bob broadcasting this new pending unrevoked
	// commitment.
	bobPendingCommit, err := aliceChannel.State().RemoteCommitChainTip()
	require.NoError(t, err)

	// We'll craft a fake spend notification with Bob's actual commitment.
	// The chain watcher should be able to detect that this is a pending
	// commit broadcast based on the state hints in the commitment.
	bobCommit := bobPendingCommit.Commitment.CommitTx
	bobTxHash := bobCommit.TxHash()
	bobSpend := &chainntnfs.SpendDetail{
		SpenderTxHash: &bobTxHash,
		SpendingTx:    bobCommit,
	}

	// Create a mock blockbeat and send it to Alice's BlockbeatChan.
	mockBeat := &chainio.MockBlockbeat{}

	// Mock the logger. We don't care how many times it's called as it's
	// not critical.
	mockBeat.On("logger").Return(log)

	// Mock a fake block height - this is called based on the debuglevel.
	mockBeat.On("Height").Return(int32(1)).Maybe()

	// Mock `NotifyBlockProcessed` to be call once.
	mockBeat.On("NotifyBlockProcessed",
		nil, aliceChainWatcher.quit).Return().Once()

	select {
	case aliceNotifier.SpendChan <- bobSpend:
	case <-time.After(1 * time.Second):
		t.Fatalf("unable to send spend details")
	}

	select {
	case aliceChainWatcher.BlockbeatChan <- mockBeat:
	case <-time.After(time.Second * 1):
		t.Fatalf("unable to send blockbeat")
	}

	// We should get a new spend event over the remote unilateral close
	// event channel.
	var uniClose *RemoteUnilateralCloseInfo
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

// dlpTestCase is a special struct that we'll use to generate randomized test
// cases for the main TestChainWatcherDataLossProtect test. This struct has a
// special Generate method that will generate a random state number, and a
// broadcast state number which is greater than that state number.
type dlpTestCase struct {
	BroadcastStateNum uint8
	NumUpdates        uint8
}

// executeStateTransitions execute the given number of state transitions.
// Copies of Alice's channel state before each transition (including initial
// state) are returned.
func executeStateTransitions(t *testing.T, htlcAmount lnwire.MilliSatoshi,
	aliceChannel, bobChannel *lnwallet.LightningChannel,
	numUpdates uint8) ([]*channeldb.OpenChannel, error) {

	// We'll make a copy of the channel state before each transition.
	var (
		chanStates []*channeldb.OpenChannel
	)

	state, err := copyChannelState(t, aliceChannel.State())
	if err != nil {
		return nil, err
	}

	chanStates = append(chanStates, state)

	for i := 0; i < int(numUpdates); i++ {
		addFakeHTLC(
			t, htlcAmount, uint64(i), aliceChannel, bobChannel,
		)

		err := lnwallet.ForceStateTransition(aliceChannel, bobChannel)
		if err != nil {
			return nil, err
		}

		state, err := copyChannelState(t, aliceChannel.State())
		if err != nil {
			return nil, err
		}

		chanStates = append(chanStates, state)
	}

	return chanStates, nil
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
	dlpScenario := func(t *testing.T, testCase dlpTestCase) bool {
		// First, we'll create two channels which already have
		// established a commitment contract between themselves.
		aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
			t, channeldb.SingleFunderBit,
		)
		if err != nil {
			t.Fatalf("unable to create test channels: %v", err)
		}

		// Based on the number of random updates for this state, make a
		// new HTLC to add to the commitment, and then lock in a state
		// transition.
		const htlcAmt = 1000
		states, err := executeStateTransitions(
			t, htlcAmt, aliceChannel, bobChannel,
			testCase.BroadcastStateNum,
		)
		if err != nil {
			t.Errorf("unable to trigger state "+
				"transition: %v", err)
			return false
		}

		// We'll use the state this test case wants Alice to start at.
		aliceChanState := states[testCase.NumUpdates]

		// With the channels created, we'll now create a chain watcher
		// instance which will be watching for any closes of Alice's
		// channel.
		aliceNotifier := &lnmock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		}
		aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
			chanState: aliceChanState,
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

		// We'll request a new channel event subscription from Alice's
		// chain watcher so we can be notified of our fake close below.
		chanEvents := aliceChainWatcher.SubscribeChannelEvents()

		// Otherwise, we'll feed in this new state number as a response
		// to the query, and insert the expected DLP commit point.
		dlpPoint := aliceChannel.State().RemoteCurrentRevocation
		err = aliceChanState.MarkDataLoss(dlpPoint)
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

		// Create a mock blockbeat and send it to Alice's
		// BlockbeatChan.
		mockBeat := &chainio.MockBlockbeat{}

		// Mock the logger. We don't care how many times it's called as
		// it's not critical.
		mockBeat.On("logger").Return(log)

		// Mock a fake block height - this is called based on the
		// debuglevel.
		mockBeat.On("Height").Return(int32(1)).Maybe()

		// Mock `NotifyBlockProcessed` to be call once.
		mockBeat.On("NotifyBlockProcessed",
			nil, aliceChainWatcher.quit).Return().Once()

		select {
		case aliceNotifier.SpendChan <- bobSpend:
		case <-time.After(time.Second * 1):
			t.Fatalf("failed to send spend notification")
		}

		select {
		case aliceChainWatcher.BlockbeatChan <- mockBeat:
		case <-time.After(time.Second * 1):
			t.Fatalf("unable to send blockbeat")
		}

		// We should get a new uni close resolution that indicates we
		// processed the DLP scenario.
		var uniClose *RemoteUnilateralCloseInfo
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

	testCases := []dlpTestCase{
		// For our first scenario, we'll ensure that if we're on state 1,
		// and the remote party broadcasts state 2 and we don't have a
		// pending commit for them, then we'll properly detect this as a
		// DLP scenario.
		{
			BroadcastStateNum: 2,
			NumUpdates:        1,
		},

		// We've completed a single update, but the remote party broadcasts
		// a state that's 5 states byeond our best known state. We've lost
		// data, but only partially, so we should enter a DLP secnario.
		{
			BroadcastStateNum: 6,
			NumUpdates:        1,
		},

		// Similar to the case above, but we've done more than one
		// update.
		{
			BroadcastStateNum: 6,
			NumUpdates:        3,
		},

		// We've done zero updates, but our channel peer broadcasts a
		// state beyond our knowledge.
		{
			BroadcastStateNum: 10,
			NumUpdates:        0,
		},
	}
	for _, testCase := range testCases {
		testName := fmt.Sprintf("num_updates=%v,broadcast_state_num=%v",
			testCase.NumUpdates, testCase.BroadcastStateNum)

		testCase := testCase
		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			if !dlpScenario(t, testCase) {
				t.Fatalf("test %v failed", testName)
			}
		})
	}
}

// TestChainWatcherLocalForceCloseDetect tests we're able to always detect our
// commitment output based on only the outputs present on the transaction.
func TestChainWatcherLocalForceCloseDetect(t *testing.T) {
	t.Parallel()

	// localForceCloseScenario is the primary test we'll use to execute our
	// table driven tests. We'll assert that for any number of state
	// updates, and if the commitment transaction has our output or not,
	// we're able to properly detect a local force close.
	localForceCloseScenario := func(t *testing.T, numUpdates, localState uint8,
		remoteOutputOnly, localOutputOnly bool) bool {

		// First, we'll create two channels which already have
		// established a commitment contract between themselves.
		aliceChannel, bobChannel, err := lnwallet.CreateTestChannels(
			t, channeldb.SingleFunderBit,
		)
		if err != nil {
			t.Fatalf("unable to create test channels: %v", err)
		}

		// We'll execute a number of state transitions based on the
		// randomly selected number from testing/quick. We do this to
		// get more coverage of various state hint encodings beyond 0
		// and 1.
		const htlcAmt = 1000
		states, err := executeStateTransitions(
			t, htlcAmt, aliceChannel, bobChannel, numUpdates,
		)
		if err != nil {
			t.Errorf("unable to trigger state "+
				"transition: %v", err)
			return false
		}

		// We'll use the state this test case wants Alice to start at.
		aliceChanState := states[localState]

		// With the channels created, we'll now create a chain watcher
		// instance which will be watching for any closes of Alice's
		// channel.
		aliceNotifier := &lnmock.ChainNotifier{
			SpendChan: make(chan *chainntnfs.SpendDetail),
			EpochChan: make(chan *chainntnfs.BlockEpoch),
			ConfChan:  make(chan *chainntnfs.TxConfirmation),
		}
		aliceChainWatcher, err := newChainWatcher(chainWatcherConfig{
			chanState:           aliceChanState,
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

		// We'll request a new channel event subscription from Alice's
		// chain watcher so we can be notified of our fake close below.
		chanEvents := aliceChainWatcher.SubscribeChannelEvents()

		// Next, we'll obtain Alice's commitment transaction and
		// trigger a force close. This should cause her to detect a
		// local force close, and dispatch a local close event.
		aliceCommit := aliceChannel.State().LocalCommitment.CommitTx

		// Since this is Alice's commitment, her output is always first
		// since she's the one creating the HTLCs (lower balance). In
		// order to simulate the commitment only having the remote
		// party's output, we'll remove Alice's output.
		if remoteOutputOnly {
			aliceCommit.TxOut = aliceCommit.TxOut[1:]
		}
		if localOutputOnly {
			aliceCommit.TxOut = aliceCommit.TxOut[:1]
		}

		aliceTxHash := aliceCommit.TxHash()
		aliceSpend := &chainntnfs.SpendDetail{
			SpenderTxHash: &aliceTxHash,
			SpendingTx:    aliceCommit,
		}
		// Create a mock blockbeat and send it to Alice's
		// BlockbeatChan.
		mockBeat := &chainio.MockBlockbeat{}

		// Mock the logger. We don't care how many times it's called as
		// it's not critical.
		mockBeat.On("logger").Return(log)

		// Mock a fake block height - this is called based on the
		// debuglevel.
		mockBeat.On("Height").Return(int32(1)).Maybe()

		// Mock `NotifyBlockProcessed` to be call once.
		mockBeat.On("NotifyBlockProcessed",
			nil, aliceChainWatcher.quit).Return().Once()

		select {
		case aliceNotifier.SpendChan <- aliceSpend:
		case <-time.After(time.Second * 1):
			t.Fatalf("unable to send spend notification")
		}

		select {
		case aliceChainWatcher.BlockbeatChan <- mockBeat:
		case <-time.After(time.Second * 1):
			t.Fatalf("unable to send blockbeat")
		}

		// We should get a local force close event from Alice as she
		// should be able to detect the close based on the commitment
		// outputs.
		select {
		case summary := <-chanEvents.LocalUnilateralClosure:
			resOpt := summary.LocalForceCloseSummary.
				ContractResolutions

			resolutions, err := resOpt.UnwrapOrErr(
				fmt.Errorf("resolutions not found"),
			)
			if err != nil {
				t.Fatalf("unable to get resolutions: %v", err)
			}

			// Make sure we correctly extracted the commit
			// resolution if we had a local output.
			if remoteOutputOnly {
				if resolutions.CommitResolution != nil {
					t.Fatalf("expected no commit resolution")
				}
			} else {
				if resolutions.CommitResolution == nil {
					t.Fatalf("expected commit resolution")
				}
			}

			return true

		case <-time.After(time.Second * 5):
			t.Errorf("didn't get local for close for state #%v",
				numUpdates)
			return false
		}
	}

	// For our test cases, we'll ensure that we test having a remote output
	// present and absent with non or some number of updates in the channel.
	testCases := []struct {
		numUpdates       uint8
		localState       uint8
		remoteOutputOnly bool
		localOutputOnly  bool
	}{
		{
			numUpdates:       0,
			localState:       0,
			remoteOutputOnly: true,
		},
		{
			numUpdates:       0,
			localState:       0,
			remoteOutputOnly: false,
		},
		{
			numUpdates:      0,
			localState:      0,
			localOutputOnly: true,
		},
		{
			numUpdates:       20,
			localState:       20,
			remoteOutputOnly: false,
		},
		{
			numUpdates:       20,
			localState:       20,
			remoteOutputOnly: true,
		},
		{
			numUpdates:      20,
			localState:      20,
			localOutputOnly: true,
		},
		{
			numUpdates:       20,
			localState:       5,
			remoteOutputOnly: false,
		},
		{
			numUpdates:       20,
			localState:       5,
			remoteOutputOnly: true,
		},
		{
			numUpdates:      20,
			localState:      5,
			localOutputOnly: true,
		},
	}
	for _, testCase := range testCases {
		testName := fmt.Sprintf(
			"num_updates=%v,remote_output=%v,local_output=%v",
			testCase.numUpdates, testCase.remoteOutputOnly,
			testCase.localOutputOnly,
		)

		testCase := testCase
		t.Run(testName, func(t *testing.T) {
			t.Parallel()

			localForceCloseScenario(
				t, testCase.numUpdates, testCase.localState,
				testCase.remoteOutputOnly,
				testCase.localOutputOnly,
			)
		})
	}
}
