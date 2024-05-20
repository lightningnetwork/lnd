package sweep

import (
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	errDummy = errors.New("dummy error")

	testPubKey, _ = btcec.ParsePubKey([]byte{
		0x04, 0x11, 0xdb, 0x93, 0xe1, 0xdc, 0xdb, 0x8a,
		0x01, 0x6b, 0x49, 0x84, 0x0f, 0x8c, 0x53, 0xbc, 0x1e,
		0xb6, 0x8a, 0x38, 0x2e, 0x97, 0xb1, 0x48, 0x2e, 0xca,
		0xd7, 0xb1, 0x48, 0xa6, 0x90, 0x9a, 0x5c, 0xb2, 0xe0,
		0xea, 0xdd, 0xfb, 0x84, 0xcc, 0xf9, 0x74, 0x44, 0x64,
		0xf8, 0x2e, 0x16, 0x0b, 0xfa, 0x9b, 0x8b, 0x64, 0xf9,
		0xd4, 0xc0, 0x3f, 0x99, 0x9b, 0x86, 0x43, 0xf6, 0x56,
		0xb4, 0x12, 0xa3,
	})
)

// TestMarkInputsPendingPublish checks that given a list of inputs with
// different states, only the non-terminal state will be marked as `Published`.
func TestMarkInputsPendingPublish(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `pendingInputs` map.
	inputNotExist := &input.MockInput{}
	defer inputNotExist.AssertExpectations(t)

	inputNotExist.On("OutPoint").Return(wire.OutPoint{Index: 0})

	// inputInit specifies a newly created input.
	inputInit := &input.MockInput{}
	defer inputInit.AssertExpectations(t)

	inputInit.On("OutPoint").Return(wire.OutPoint{Index: 1})

	s.inputs[inputInit.OutPoint()] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &input.MockInput{}
	defer inputPendingPublish.AssertExpectations(t)

	inputPendingPublish.On("OutPoint").Return(wire.OutPoint{Index: 2})

	s.inputs[inputPendingPublish.OutPoint()] = &SweeperInput{
		state: PendingPublish,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &input.MockInput{}
	defer inputTerminated.AssertExpectations(t)

	inputTerminated.On("OutPoint").Return(wire.OutPoint{Index: 3})

	s.inputs[inputTerminated.OutPoint()] = &SweeperInput{
		state: Excluded,
	}

	// Mark the test inputs. We expect the non-exist input and the
	// inputTerminated to be skipped, and the rest to be marked as pending
	// publish.
	set.On("Inputs").Return([]input.Input{
		inputNotExist, inputInit, inputPendingPublish, inputTerminated,
	})
	s.markInputsPendingPublish(set)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 3)

	// We expect the init input's state to become pending publish.
	require.Equal(PendingPublish, s.inputs[inputInit.OutPoint()].state)

	// We expect the pending-publish to stay unchanged.
	require.Equal(PendingPublish,
		s.inputs[inputPendingPublish.OutPoint()].state)

	// We expect the terminated to stay unchanged.
	require.Equal(Excluded, s.inputs[inputTerminated.OutPoint()].state)
}

// TestMarkInputsPublished checks that given a list of inputs with different
// states, only the state `PendingPublish` will be marked as `Published`.
func TestMarkInputsPublished(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock sweeper store.
	mockStore := NewMockSweeperStore()

	// Create a test TxRecord and a dummy error.
	dummyTR := &TxRecord{}
	dummyErr := errors.New("dummy error")

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
	}

	// First, check that when an error is returned from db, it's properly
	// returned here.
	mockStore.On("StoreTx", dummyTR).Return(dummyErr).Once()
	err := s.markInputsPublished(dummyTR, nil)
	require.ErrorIs(err, dummyErr)

	// We also expect the record has been marked as published.
	require.True(dummyTR.Published)

	// Then, check that the target input has will be correctly marked as
	// published.
	//
	// Mock the store to return nil
	mockStore.On("StoreTx", dummyTR).Return(nil).Once()

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	err = s.markInputsPublished(dummyTR, []*wire.TxIn{
		inputNotExist, inputInit, inputPendingPublish,
	})
	require.NoError(err)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 2)

	// We expect the init input's state to stay unchanged.
	require.Equal(Init,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as published.
	require.Equal(Published,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsPublishFailed checks that given a list of inputs with
// different states, only the state `PendingPublish` and `Published` will be
// marked as `PublishFailed`.
func TestMarkInputsPublishFailed(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock sweeper store.
	mockStore := NewMockSweeperStore()

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create testing inputs for each state.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input. When marking this as
	// published, we should see an error log as this input hasn't been
	// published yet.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
	}

	// inputPublished specifies an input that's published.
	inputPublished := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.inputs[inputPublished.PreviousOutPoint] = &SweeperInput{
		state: Published,
	}

	// inputPublishFailed specifies an input that's failed to be published.
	inputPublishFailed := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 5},
	}
	s.inputs[inputPublishFailed.PreviousOutPoint] = &SweeperInput{
		state: PublishFailed,
	}

	// inputSwept specifies an input that's swept.
	inputSwept := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 6},
	}
	s.inputs[inputSwept.PreviousOutPoint] = &SweeperInput{
		state: Swept,
	}

	// inputExcluded specifies an input that's excluded.
	inputExcluded := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 7},
	}
	s.inputs[inputExcluded.PreviousOutPoint] = &SweeperInput{
		state: Excluded,
	}

	// inputFailed specifies an input that's failed.
	inputFailed := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 8},
	}
	s.inputs[inputFailed.PreviousOutPoint] = &SweeperInput{
		state: Failed,
	}

	// Gather all inputs' outpoints.
	pendingOps := make([]wire.OutPoint, 0, len(s.inputs)+1)
	for op := range s.inputs {
		pendingOps = append(pendingOps, op)
	}
	pendingOps = append(pendingOps, inputNotExist.PreviousOutPoint)

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	s.markInputsPublishFailed(pendingOps)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 7)

	// We expect the init input's state to stay unchanged.
	require.Equal(Init,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish input's is now marked as publish
	// failed.
	require.Equal(PublishFailed,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the published input's is now marked as publish failed.
	require.Equal(PublishFailed,
		s.inputs[inputPublished.PreviousOutPoint].state)

	// We expect the publish failed input to stay unchanged.
	require.Equal(PublishFailed,
		s.inputs[inputPublishFailed.PreviousOutPoint].state)

	// We expect the swept input to stay unchanged.
	require.Equal(Swept, s.inputs[inputSwept.PreviousOutPoint].state)

	// We expect the excluded input to stay unchanged.
	require.Equal(Excluded, s.inputs[inputExcluded.PreviousOutPoint].state)

	// We expect the failed input to stay unchanged.
	require.Equal(Failed, s.inputs[inputFailed.PreviousOutPoint].state)

	// Assert mocked statements are executed as expected.
	mockStore.AssertExpectations(t)
}

// TestMarkInputsSwept checks that given a list of inputs with different
// states, only the non-terminal state will be marked as `Swept`.
func TestMarkInputsSwept(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create three testing inputs.
	//
	// inputNotExist specifies an input that's not found in the sweeper's
	// `inputs` map.
	inputNotExist := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	}

	// inputInit specifies a newly created input.
	inputInit := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 2},
	}
	s.inputs[inputInit.PreviousOutPoint] = &SweeperInput{
		state: Init,
		Input: mockInput,
	}

	// inputPendingPublish specifies an input that's about to be published.
	inputPendingPublish := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 3},
	}
	s.inputs[inputPendingPublish.PreviousOutPoint] = &SweeperInput{
		state: PendingPublish,
		Input: mockInput,
	}

	// inputTerminated specifies an input that's terminated.
	inputTerminated := &wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 4},
	}
	s.inputs[inputTerminated.PreviousOutPoint] = &SweeperInput{
		state: Excluded,
		Input: mockInput,
	}

	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			inputNotExist, inputInit,
			inputPendingPublish, inputTerminated,
		},
	}

	// Mark the test inputs. We expect the inputTerminated to be skipped,
	// and the rest to be marked as swept.
	s.markInputsSwept(tx, true)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 3)

	// We expect the init input's state to become swept.
	require.Equal(Swept,
		s.inputs[inputInit.PreviousOutPoint].state)

	// We expect the pending-publish becomes swept.
	require.Equal(Swept,
		s.inputs[inputPendingPublish.PreviousOutPoint].state)

	// We expect the terminated to stay unchanged.
	require.Equal(Excluded,
		s.inputs[inputTerminated.PreviousOutPoint].state)
}

// TestMempoolLookup checks that the method `mempoolLookup` works as expected.
func TestMempoolLookup(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test outpoint.
	op := wire.OutPoint{Index: 1}

	// Create a mock mempool watcher.
	mockMempool := chainntnfs.NewMockMempoolWatcher()
	defer mockMempool.AssertExpectations(t)

	// Create a test sweeper without a mempool.
	s := New(&UtxoSweeperConfig{})

	// Since we don't have a mempool, we expect the call to return a
	// fn.None indicating it's not found.
	tx := s.mempoolLookup(op)
	require.True(tx.IsNone())

	// Re-create the sweeper with the mocked mempool watcher.
	s = New(&UtxoSweeperConfig{
		Mempool: mockMempool,
	})

	// Mock the mempool watcher to return not found.
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.None[wire.MsgTx]()).Once()

	// We expect a fn.None tx to be returned.
	tx = s.mempoolLookup(op)
	require.True(tx.IsNone())

	// Mock the mempool to return a spending tx.
	dummyTx := wire.MsgTx{}
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.Some(dummyTx)).Once()

	// Calling the loopup again, we expect the dummyTx to be returned.
	tx = s.mempoolLookup(op)
	require.False(tx.IsNone())
	require.Equal(dummyTx, tx.UnsafeFromSome())
}

// TestUpdateSweeperInputs checks that the method `updateSweeperInputs` will
// properly update the inputs based on their states.
func TestUpdateSweeperInputs(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test sweeper.
	s := New(nil)

	// Create mock inputs.
	inp1 := &input.MockInput{}
	defer inp1.AssertExpectations(t)
	inp2 := &input.MockInput{}
	defer inp2.AssertExpectations(t)
	inp3 := &input.MockInput{}
	defer inp3.AssertExpectations(t)

	// Create a list of inputs using all the states.
	//
	// Mock the input to have a locktime that's matured so it will be
	// returned.
	inp1.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp1.On("BlocksToMaturity").Return(uint32(0)).Once()
	inp1.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input0 := &SweeperInput{state: Init, Input: inp1}

	// These inputs won't hit RequiredLockTime so we won't mock.
	input1 := &SweeperInput{state: PendingPublish, Input: inp1}
	input2 := &SweeperInput{state: Published, Input: inp1}

	// Mock the input to have a locktime that's matured so it will be
	// returned.
	inp1.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp1.On("BlocksToMaturity").Return(uint32(0)).Once()
	inp1.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input3 := &SweeperInput{state: PublishFailed, Input: inp1}

	// These inputs won't hit RequiredLockTime so we won't mock.
	input4 := &SweeperInput{state: Swept, Input: inp1}
	input5 := &SweeperInput{state: Excluded, Input: inp1}
	input6 := &SweeperInput{state: Failed, Input: inp1}

	// Mock the input to have a locktime in the future so it will NOT be
	// returned.
	inp2.On("RequiredLockTime").Return(
		uint32(s.currentHeight+1), true).Once()
	input7 := &SweeperInput{state: Init, Input: inp2}

	// Mock the input to have a CSV expiry in the future so it will NOT be
	// returned.
	inp3.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp3.On("BlocksToMaturity").Return(uint32(2)).Once()
	inp3.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	input8 := &SweeperInput{state: Init, Input: inp3}

	// Add the inputs to the sweeper. After the update, we should see the
	// terminated inputs being removed.
	s.inputs = map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
		{Index: 4}: input4,
		{Index: 5}: input5,
		{Index: 6}: input6,
		{Index: 7}: input7,
		{Index: 8}: input8,
	}

	// We expect the inputs with `Swept`, `Excluded`, and `Failed` to be
	// removed.
	expectedInputs := map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 1}: input1,
		{Index: 2}: input2,
		{Index: 3}: input3,
		{Index: 7}: input7,
		{Index: 8}: input8,
	}

	// We expect only the inputs with `Init` and `PublishFailed` to be
	// returned.
	expectedReturn := map[wire.OutPoint]*SweeperInput{
		{Index: 0}: input0,
		{Index: 3}: input3,
	}

	// Update the sweeper inputs.
	inputs := s.updateSweeperInputs()

	// Assert the returned inputs are as expected.
	require.Equal(expectedReturn, inputs)

	// Assert the sweeper inputs are as expected.
	require.Equal(expectedInputs, s.inputs)
}

// TestDecideStateAndRBFInfo checks that the expected state and RBFInfo are
// returned based on whether this input can be found both in mempool and the
// sweeper store.
func TestDecideStateAndRBFInfo(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a test outpoint.
	op := wire.OutPoint{Index: 1}

	// Create a mock mempool watcher and a mock sweeper store.
	mockMempool := chainntnfs.NewMockMempoolWatcher()
	defer mockMempool.AssertExpectations(t)
	mockStore := NewMockSweeperStore()
	defer mockStore.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:   mockStore,
		Mempool: mockMempool,
	})

	// First, mock the mempool to return false.
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.None[wire.MsgTx]()).Once()

	// Since the mempool lookup failed, we exepect state Init and no
	// RBFInfo.
	state, rbf := s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Init, state)

	// Mock the mempool lookup to return a tx three times as we are calling
	// attachAvailableRBFInfo three times.
	tx := wire.MsgTx{}
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.Some(tx)).Times(3)

	// Mock the store to return an error saying the tx cannot be found.
	mockStore.On("GetTx", tx.TxHash()).Return(nil, ErrTxNotFound).Once()

	// Although the db lookup failed, we expect the state to be Published.
	state, rbf = s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Published, state)

	// Mock the store to return a db error.
	dummyErr := errors.New("dummy error")
	mockStore.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Although the db lookup failed, we expect the state to be Published.
	state, rbf = s.decideStateAndRBFInfo(op)
	require.True(rbf.IsNone())
	require.Equal(Published, state)

	// Mock the store to return a record.
	tr := &TxRecord{
		Fee:     100,
		FeeRate: 100,
	}
	mockStore.On("GetTx", tx.TxHash()).Return(tr, nil).Once()

	// Call the method again.
	state, rbf = s.decideStateAndRBFInfo(op)

	// Assert that the RBF info is returned.
	rbfInfo := fn.Some(RBFInfo{
		Txid:    tx.TxHash(),
		Fee:     btcutil.Amount(tr.Fee),
		FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
	})
	require.Equal(rbfInfo, rbf)

	// Assert the state is updated.
	require.Equal(Published, state)
}

// TestMarkInputFailed checks that the input is marked as failed as expected.
func TestMarkInputFailed(t *testing.T) {
	t.Parallel()

	// Create a mock input.
	mockInput := &input.MockInput{}
	defer mockInput.AssertExpectations(t)

	// Mock the `OutPoint` to return a dummy outpoint.
	mockInput.On("OutPoint").Return(wire.OutPoint{Hash: chainhash.Hash{1}})

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create a testing pending input.
	pi := &SweeperInput{
		state: Init,
		Input: mockInput,
	}

	// Call the method under test.
	s.markInputFailed(pi, errors.New("dummy error"))

	// Assert the state is updated.
	require.Equal(t, Failed, pi.state)
}

// TestSweepPendingInputs checks that `sweepPendingInputs` correctly executes
// its workflow based on the returned values from the interfaces.
func TestSweepPendingInputs(t *testing.T) {
	t.Parallel()

	// Create a mock wallet and aggregator.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	aggregator := &mockUtxoAggregator{}
	defer aggregator.AssertExpectations(t)

	publisher := &MockBumper{}
	defer publisher.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Wallet:     wallet,
		Aggregator: aggregator,
		Publisher:  publisher,
		GenSweepScript: func() ([]byte, error) {
			return testPubKey.SerializeCompressed(), nil
		},
		NoDeadlineConfTarget: uint32(DefaultDeadlineDelta),
	})

	// Set a current height to test the deadline override.
	s.currentHeight = testHeight

	// Create an input set that needs wallet inputs.
	setNeedWallet := &MockInputSet{}
	defer setNeedWallet.AssertExpectations(t)

	// Mock this set to ask for wallet input.
	setNeedWallet.On("NeedWalletInput").Return(true).Once()
	setNeedWallet.On("AddWalletInputs", wallet).Return(nil).Once()

	// Mock the wallet to require the lock once.
	wallet.On("WithCoinSelectLock", mock.Anything).Return(nil).Once()

	// Create an input set that doesn't need wallet inputs.
	normalSet := &MockInputSet{}
	defer normalSet.AssertExpectations(t)

	normalSet.On("NeedWalletInput").Return(false).Once()

	// Mock the methods used in `sweep`. This is not important for this
	// unit test.
	setNeedWallet.On("Inputs").Return(nil).Maybe()
	setNeedWallet.On("DeadlineHeight").Return(testHeight).Once()
	setNeedWallet.On("Budget").Return(btcutil.Amount(1)).Once()
	setNeedWallet.On("StartingFeeRate").Return(
		fn.None[chainfee.SatPerKWeight]()).Once()
	normalSet.On("Inputs").Return(nil).Maybe()
	normalSet.On("DeadlineHeight").Return(testHeight).Once()
	normalSet.On("Budget").Return(btcutil.Amount(1)).Once()
	normalSet.On("StartingFeeRate").Return(
		fn.None[chainfee.SatPerKWeight]()).Once()

	// Make pending inputs for testing. We don't need real values here as
	// the returned clusters are mocked.
	pis := make(InputsMap)

	// Mock the aggregator to return the mocked input sets.
	aggregator.On("ClusterInputs", pis).Return([]InputSet{
		setNeedWallet, normalSet,
	})

	// Mock `Broadcast` to return an error. This should cause the
	// `createSweepTx` inside `sweep` to fail. This is done so we can
	// terminate the method early as we are only interested in testing the
	// workflow in `sweepPendingInputs`. We don't need to test `sweep` here
	// as it should be tested in its own unit test.
	dummyErr := errors.New("dummy error")
	publisher.On("Broadcast", mock.Anything).Return(nil, dummyErr).Twice()

	// Call the method under test.
	s.sweepPendingInputs(pis)
}

// TestHandleBumpEventTxFailed checks that the sweeper correctly handles the
// case where the bump event tx fails to be published.
func TestHandleBumpEventTxFailed(t *testing.T) {
	t.Parallel()

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	var (
		// Create four testing outpoints.
		op1        = wire.OutPoint{Hash: chainhash.Hash{1}}
		op2        = wire.OutPoint{Hash: chainhash.Hash{2}}
		op3        = wire.OutPoint{Hash: chainhash.Hash{3}}
		opNotExist = wire.OutPoint{Hash: chainhash.Hash{4}}
	)

	// Create three mock inputs.
	input1 := &input.MockInput{}
	defer input1.AssertExpectations(t)

	input2 := &input.MockInput{}
	defer input2.AssertExpectations(t)

	input3 := &input.MockInput{}
	defer input3.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op1: &SweeperInput{Input: input1, state: PendingPublish},
		op2: &SweeperInput{Input: input2, state: PendingPublish},
		op3: &SweeperInput{Input: input3, state: PendingPublish},
	}

	// Create a testing tx that spends the first two inputs.
	tx := &wire.MsgTx{
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op1},
			{PreviousOutPoint: op2},
			{PreviousOutPoint: opNotExist},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxFailed,
		Err:   errDummy,
	}

	// Call the method under test.
	err := s.handleBumpEvent(br)
	require.ErrorIs(t, err, errDummy)

	// Assert the states of the first two inputs are updated.
	require.Equal(t, PublishFailed, s.inputs[op1].state)
	require.Equal(t, PublishFailed, s.inputs[op2].state)

	// Assert the state of the third input is not updated.
	require.Equal(t, PendingPublish, s.inputs[op3].state)

	// Assert the non-existing input is not added to the pending inputs.
	require.NotContains(t, s.inputs, opNotExist)
}

// TestHandleBumpEventTxReplaced checks that the sweeper correctly handles the
// case where the bump event tx is replaced.
func TestHandleBumpEventTxReplaced(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock wallet.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a replacement tx.
	replacementTx := &wire.MsgTx{
		LockTime: 2,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:         replacementTx,
		ReplacedTx: tx,
		Event:      TxReplaced,
	}

	// Mock the store to return an error.
	dummyErr := errors.New("dummy error")
	store.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Call the method under test and assert the error is returned.
	err := s.handleBumpEventTxReplaced(br)
	require.ErrorIs(t, err, dummyErr)

	// Mock the store to return the old tx record.
	store.On("GetTx", tx.TxHash()).Return(&TxRecord{
		Txid: tx.TxHash(),
	}, nil).Once()

	// We expect to cancel rebroadcasting the replaced tx.
	wallet.On("CancelRebroadcast", tx.TxHash()).Once()

	// Mock an error returned when deleting the old tx record.
	store.On("DeleteTx", tx.TxHash()).Return(dummyErr).Once()

	// Call the method under test and assert the error is returned.
	err = s.handleBumpEventTxReplaced(br)
	require.ErrorIs(t, err, dummyErr)

	// Mock the store to return the old tx record and delete it without
	// error.
	store.On("GetTx", tx.TxHash()).Return(&TxRecord{
		Txid: tx.TxHash(),
	}, nil).Once()
	store.On("DeleteTx", tx.TxHash()).Return(nil).Once()

	// Mock the store to save the new tx record.
	store.On("StoreTx", &TxRecord{
		Txid:      replacementTx.TxHash(),
		Published: true,
	}).Return(nil).Once()

	// We expect to cancel rebroadcasting the replaced tx.
	wallet.On("CancelRebroadcast", tx.TxHash()).Once()

	// Call the method under test.
	err = s.handleBumpEventTxReplaced(br)
	require.NoError(t, err)

	// Assert the state of the input is updated.
	require.Equal(t, Published, s.inputs[op].state)
}

// TestHandleBumpEventTxPublished checks that the sweeper correctly handles the
// case where the bump event tx is published.
func TestHandleBumpEventTxPublished(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxPublished,
	}

	// Mock the store to save the new tx record.
	store.On("StoreTx", &TxRecord{
		Txid:      tx.TxHash(),
		Published: true,
	}).Return(nil).Once()

	// Call the method under test.
	err := s.handleBumpEventTxPublished(br)
	require.NoError(t, err)

	// Assert the state of the input is updated.
	require.Equal(t, Published, s.inputs[op].state)
}

// TestMonitorFeeBumpResult checks that the fee bump monitor loop correctly
// exits when the sweeper is stopped, the tx is confirmed or failed.
func TestMonitorFeeBumpResult(t *testing.T) {
	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock wallet.
	wallet := &MockWallet{}
	defer wallet.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a testing outpoint.
	op := wire.OutPoint{Hash: chainhash.Hash{1}}

	// Create a mock input.
	inp := &input.MockInput{}
	defer inp.AssertExpectations(t)

	// Construct the initial state for the sweeper.
	s.inputs = InputsMap{
		op: &SweeperInput{Input: inp, state: PendingPublish},
	}

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}

	testCases := []struct {
		name            string
		setupResultChan func() <-chan *BumpResult
		shouldExit      bool
	}{
		{
			// When a tx confirmed event is received, we expect to
			// exit the monitor loop.
			name: "tx confirmed",
			// We send a result with TxConfirmed event to the
			// result channel.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:      tx,
					Event:   TxConfirmed,
					Fee:     10000,
					FeeRate: 100,
				}

				// We expect to cancel rebroadcasting the tx
				// once confirmed.
				wallet.On("CancelRebroadcast",
					tx.TxHash()).Once()

				return resultChan
			},
			shouldExit: true,
		},
		{
			// When a tx failed event is received, we expect to
			// exit the monitor loop.
			name: "tx failed",
			// We send a result with TxConfirmed event to the
			// result channel.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:    tx,
					Event: TxFailed,
					Err:   errDummy,
				}

				// We expect to cancel rebroadcasting the tx
				// once failed.
				wallet.On("CancelRebroadcast",
					tx.TxHash()).Once()

				return resultChan
			},
			shouldExit: true,
		},
		{
			// When processing non-confirmed events, the monitor
			// should not exit.
			name: "no exit on normal event",
			// We send a result with TxPublished and mock the
			// method `StoreTx` to return nil.
			setupResultChan: func() <-chan *BumpResult {
				// Create a result chan.
				resultChan := make(chan *BumpResult, 1)
				resultChan <- &BumpResult{
					Tx:    tx,
					Event: TxPublished,
				}

				return resultChan
			},
			shouldExit: false,
		}, {
			// When the sweeper is shutting down, the monitor loop
			// should exit.
			name: "exit on sweeper shutdown",
			// We don't send anything but quit the sweeper.
			setupResultChan: func() <-chan *BumpResult {
				close(s.quit)

				return nil
			},
			shouldExit: true,
		},
	}

	for _, tc := range testCases {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			// Setup the testing result channel.
			resultChan := tc.setupResultChan()

			// Create a done chan that's used to signal the monitor
			// has exited.
			done := make(chan struct{})

			s.wg.Add(1)
			go func() {
				s.monitorFeeBumpResult(resultChan)
				close(done)
			}()

			// The monitor is expected to exit, we check it's done
			// in one second or fail.
			if tc.shouldExit {
				select {
				case <-done:
				case <-time.After(1 * time.Second):
					require.Fail(t, "monitor not exited")
				}

				return
			}

			// The monitor should not exit, check it doesn't close
			// the `done` channel within one second.
			select {
			case <-done:
				require.Fail(t, "monitor exited")
			case <-time.After(1 * time.Second):
			}
		})
	}
}
