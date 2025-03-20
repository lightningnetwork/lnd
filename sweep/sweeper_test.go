package sweep

import (
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
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

// createMockInput creates a mock input and saves it to the sweeper's inputs
// map. The created input has the specified state and a random outpoint. It
// will assert the method `OutPoint` is called at least once.
func createMockInput(t *testing.T, s *UtxoSweeper,
	state SweepState) *input.MockInput {

	inp := &input.MockInput{}
	t.Cleanup(func() {
		inp.AssertExpectations(t)
	})

	randBuf := make([]byte, lntypes.HashSize)
	_, err := rand.Read(randBuf)
	require.NoError(t, err, "internal error, cannot generate random bytes")

	randHash, err := chainhash.NewHash(randBuf)
	require.NoError(t, err)

	inp.On("OutPoint").Return(wire.OutPoint{
		Hash:  *randHash,
		Index: 0,
	})

	// We don't do branch switches based on the witness type here so we
	// just mock it.
	inp.On("WitnessType").Return(input.CommitmentTimeLock).Maybe()

	s.inputs[inp.OutPoint()] = &SweeperInput{
		Input: inp,
		state: state,
	}

	return inp
}

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

	// Create three inputs with different states.
	// - inputInit specifies a newly created input.
	// - inputPendingPublish specifies an input about to be published.
	// - inputTerminated specifies an input that's terminated.
	var (
		inputInit           = createMockInput(t, s, Init)
		inputPendingPublish = createMockInput(t, s, PendingPublish)
		inputTerminated     = createMockInput(t, s, Excluded)
	)

	// Mark the test inputs. We expect the non-exist input and the
	// inputTerminated to be skipped, and the rest to be marked as pending
	// publish.
	set.On("Inputs").Return([]input.Input{
		inputInit, inputPendingPublish, inputTerminated,
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

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create two inputs with different states.
	// - inputInit specifies a newly created input.
	// - inputPendingPublish specifies an input about to be published.
	var (
		inputInit           = createMockInput(t, s, Init)
		inputPendingPublish = createMockInput(t, s, PendingPublish)
	)

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
	set.On("Inputs").Return([]input.Input{inputInit, inputPendingPublish})

	err = s.markInputsPublished(dummyTR, set)
	require.NoError(err)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 2)

	// We expect the init input's state to stay unchanged.
	require.Equal(Init,
		s.inputs[inputInit.OutPoint()].state)

	// We expect the pending-publish input's is now marked as published.
	require.Equal(Published,
		s.inputs[inputPendingPublish.OutPoint()].state)

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

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: mockStore,
	})

	// Create inputs with different states.
	// - inputInit specifies a newly created input. When marking this as
	//   published, we should see an error log as this input hasn't been
	//   published yet.
	// - inputPendingPublish specifies an input about to be published.
	// - inputPublished specifies an input that's published.
	// - inputPublishFailed specifies an input that's failed to be
	//   published.
	// - inputSwept specifies an input that's swept.
	// - inputExcluded specifies an input that's excluded.
	// - inputFatal specifies an input that's fatal.
	var (
		inputInit           = createMockInput(t, s, Init)
		inputPendingPublish = createMockInput(t, s, PendingPublish)
		inputPublished      = createMockInput(t, s, Published)
		inputPublishFailed  = createMockInput(t, s, PublishFailed)
		inputSwept          = createMockInput(t, s, Swept)
		inputExcluded       = createMockInput(t, s, Excluded)
		inputFatal          = createMockInput(t, s, Fatal)
	)

	// Gather all inputs.
	set.On("Inputs").Return([]input.Input{
		inputInit, inputPendingPublish, inputPublished,
		inputPublishFailed, inputSwept, inputExcluded, inputFatal,
	})

	feeRate := chainfee.SatPerKWeight(1000)

	// Mark the test inputs. We expect the non-exist input and the
	// inputInit to be skipped, and the final input to be marked as
	// published.
	s.markInputsPublishFailed(set, feeRate)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 7)

	// We expect the init input's state to stay unchanged.
	pi := s.inputs[inputInit.OutPoint()]
	require.Equal(Init, pi.state)
	require.True(pi.params.StartingFeeRate.IsNone())

	// We expect the pending-publish input's is now marked as publish
	// failed.
	pi = s.inputs[inputPendingPublish.OutPoint()]
	require.Equal(PublishFailed, pi.state)
	require.Equal(feeRate, pi.params.StartingFeeRate.UnsafeFromSome())

	// We expect the published input's is now marked as publish failed.
	pi = s.inputs[inputPublished.OutPoint()]
	require.Equal(PublishFailed, pi.state)
	require.Equal(feeRate, pi.params.StartingFeeRate.UnsafeFromSome())

	// We expect the publish failed input to stay unchanged.
	pi = s.inputs[inputPublishFailed.OutPoint()]
	require.Equal(PublishFailed, pi.state)
	require.True(pi.params.StartingFeeRate.IsNone())

	// We expect the swept input to stay unchanged.
	pi = s.inputs[inputSwept.OutPoint()]
	require.Equal(Swept, pi.state)
	require.True(pi.params.StartingFeeRate.IsNone())

	// We expect the excluded input to stay unchanged.
	pi = s.inputs[inputExcluded.OutPoint()]
	require.Equal(Excluded, pi.state)
	require.True(pi.params.StartingFeeRate.IsNone())

	// We expect the fatal input to stay unchanged.
	pi = s.inputs[inputFatal.OutPoint()]
	require.Equal(Fatal, pi.state)
	require.True(pi.params.StartingFeeRate.IsNone())

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
	input6 := &SweeperInput{state: Fatal, Input: inp1}

	// Mock the input to have a locktime in the future so it will NOT be
	// returned.
	inp2.On("RequiredLockTime").Return(
		uint32(s.currentHeight+1), true).Once()
	inp2.On("OutPoint").Return(wire.OutPoint{Index: 2}).Maybe()
	input7 := &SweeperInput{state: Init, Input: inp2}

	// Mock the input to have a CSV expiry in the future so it will NOT be
	// returned.
	inp3.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp3.On("BlocksToMaturity").Return(uint32(2)).Once()
	inp3.On("HeightHint").Return(uint32(s.currentHeight)).Once()
	inp3.On("OutPoint").Return(wire.OutPoint{Index: 3}).Maybe()
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

// TestDecideRBFInfo checks that the expected RBFInfo is returned based on
// whether this input can be found both in mempool and the sweeper store.
func TestDecideRBFInfo(t *testing.T) {
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

	// Since the mempool lookup failed, we expect no RBFInfo.
	rbf := s.decideRBFInfo(op)
	require.True(rbf.IsNone())

	// Mock the mempool lookup to return a tx three times as we are calling
	// attachAvailableRBFInfo three times.
	tx := wire.MsgTx{}
	mockMempool.On("LookupInputMempoolSpend", op).Return(
		fn.Some(tx)).Times(3)

	// Mock the store to return an error saying the tx cannot be found.
	mockStore.On("GetTx", tx.TxHash()).Return(nil, ErrTxNotFound).Once()

	// The db lookup failed, we expect no RBFInfo.
	rbf = s.decideRBFInfo(op)
	require.True(rbf.IsNone())

	// Mock the store to return a db error.
	dummyErr := errors.New("dummy error")
	mockStore.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// The db lookup failed, we expect no RBFInfo.
	rbf = s.decideRBFInfo(op)
	require.True(rbf.IsNone())

	// Mock the store to return a record.
	tr := &TxRecord{
		Fee:     100,
		FeeRate: 100,
	}
	mockStore.On("GetTx", tx.TxHash()).Return(tr, nil).Once()

	// Call the method again.
	rbf = s.decideRBFInfo(op)

	// Assert that the RBF info is returned.
	rbfInfo := fn.Some(RBFInfo{
		Txid:    tx.TxHash(),
		Fee:     btcutil.Amount(tr.Fee),
		FeeRate: chainfee.SatPerKWeight(tr.FeeRate),
	})
	require.Equal(rbfInfo, rbf)
}

// TestMarkInputFatal checks that the input is marked as expected.
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
	s.markInputFatal(pi, nil, errors.New("dummy error"))

	// Assert the state is updated.
	require.Equal(t, Fatal, pi.state)
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
		GenSweepScript: func() fn.Result[lnwallet.AddrWithKey] {
			//nolint:ll
			return fn.Ok(lnwallet.AddrWithKey{
				DeliveryAddress: testPubKey.SerializeCompressed(),
			})
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
	setNeedWallet.On("Immediate").Return(false).Once()
	normalSet.On("Inputs").Return(nil).Maybe()
	normalSet.On("DeadlineHeight").Return(testHeight).Once()
	normalSet.On("Budget").Return(btcutil.Amount(1)).Once()
	normalSet.On("StartingFeeRate").Return(
		fn.None[chainfee.SatPerKWeight]()).Once()
	normalSet.On("Immediate").Return(false).Once()

	// Make pending inputs for testing. We don't need real values here as
	// the returned clusters are mocked.
	pis := make(InputsMap)

	// Mock the aggregator to return the mocked input sets.
	aggregator.On("ClusterInputs", pis).Return([]InputSet{
		setNeedWallet, normalSet,
	})

	// Mock `Broadcast` to return a result.
	publisher.On("Broadcast", mock.Anything).Return(nil).Twice()

	// Call the method under test.
	s.sweepPendingInputs(pis)
}

// TestHandleBumpEventTxFailed checks that the sweeper correctly handles the
// case where the bump event tx fails to be published.
func TestHandleBumpEventTxFailed(t *testing.T) {
	t.Parallel()

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// inputNotExist specifies an input that's not found in the sweeper's
	// `pendingInputs` map.
	inputNotExist := &input.MockInput{}
	defer inputNotExist.AssertExpectations(t)
	inputNotExist.On("OutPoint").Return(wire.OutPoint{Index: 0})
	opNotExist := inputNotExist.OutPoint()

	// Create three mock inputs.
	var (
		input1 = createMockInput(t, s, PendingPublish)
		input2 = createMockInput(t, s, PendingPublish)
		input3 = createMockInput(t, s, PendingPublish)
	)

	op1 := input1.OutPoint()
	op2 := input2.OutPoint()
	op3 := input3.OutPoint()

	// Construct the initial state for the sweeper.
	set.On("Inputs").Return([]input.Input{input1, input2, input3})

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

	// Create a testing bump response.
	resp := &bumpResp{
		result: br,
		set:    set,
	}

	// Call the method under test.
	err := s.handleBumpEvent(resp)
	require.NoError(t, err)

	// Assert the states of the first two inputs are updated.
	require.Equal(t, PublishFailed, s.inputs[op1].state)
	require.Equal(t, PublishFailed, s.inputs[op2].state)

	// Assert the state of the third input.
	//
	// NOTE: Although the tx doesn't spend it, we still mark this input as
	// failed as we are treating the input set as the single source of
	// truth.
	require.Equal(t, PublishFailed, s.inputs[op3].state)

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

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PendingPublish)
	set.On("Inputs").Return([]input.Input{inp})

	op := inp.OutPoint()

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

	// Create a testing bump response.
	resp := &bumpResp{
		result: br,
		set:    set,
	}

	// Mock the store to return an error.
	dummyErr := errors.New("dummy error")
	store.On("GetTx", tx.TxHash()).Return(nil, dummyErr).Once()

	// Call the method under test and assert the error is returned.
	err := s.handleBumpEventTxReplaced(resp)
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
	err = s.handleBumpEventTxReplaced(resp)
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
	err = s.handleBumpEventTxReplaced(resp)
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

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PendingPublish)
	set.On("Inputs").Return([]input.Input{inp})

	op := inp.OutPoint()

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

	// Create a testing bump response.
	resp := &bumpResp{
		result: br,
		set:    set,
	}

	// Mock the store to save the new tx record.
	store.On("StoreTx", &TxRecord{
		Txid:      tx.TxHash(),
		Published: true,
	}).Return(nil).Once()

	// Call the method under test.
	err := s.handleBumpEventTxPublished(resp)
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

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store:  store,
		Wallet: wallet,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PendingPublish)

	// Create a testing tx that spends the input.
	op := inp.OutPoint()
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
		},
		{
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
				s.monitorFeeBumpResult(set, resultChan)
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

// TestMarkInputsFailed checks that given a list of inputs with different
// states, the method `markInputsFailed` correctly marks the inputs as failed.
func TestMarkInputsFailed(t *testing.T) {
	t.Parallel()

	require := require.New(t)

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{})

	// Create testing inputs for each state.
	// - inputInit specifies a newly created input. When marking this as
	//   published, we should see an error log as this input hasn't been
	//   published yet.
	// - inputPendingPublish specifies an input about to be published.
	// - inputPublished specifies an input that's published.
	// - inputPublishFailed specifies an input that's failed to be
	//   published.
	// - inputSwept specifies an input that's swept.
	// - inputExcluded specifies an input that's excluded.
	// - inputFatal specifies an input that's fatal.
	var (
		inputInit           = createMockInput(t, s, Init)
		inputPendingPublish = createMockInput(t, s, PendingPublish)
		inputPublished      = createMockInput(t, s, Published)
		inputPublishFailed  = createMockInput(t, s, PublishFailed)
		inputSwept          = createMockInput(t, s, Swept)
		inputExcluded       = createMockInput(t, s, Excluded)
		inputFatal          = createMockInput(t, s, Fatal)
	)

	// Gather all inputs.
	set.On("Inputs").Return([]input.Input{
		inputInit, inputPendingPublish, inputPublished,
		inputPublishFailed, inputSwept, inputExcluded, inputFatal,
	})

	// Mark the test inputs. We expect the non-exist input and
	// inputSwept/inputExcluded/inputFatal to be skipped.
	s.markInputsFatal(set, errDummy)

	// We expect unchanged number of pending inputs.
	require.Len(s.inputs, 7)

	// We expect the init input's to be marked as fatal.
	require.Equal(Fatal, s.inputs[inputInit.OutPoint()].state)

	// We expect the pending-publish input to be marked as failed.
	require.Equal(Fatal, s.inputs[inputPendingPublish.OutPoint()].state)

	// We expect the published input to be marked as fatal.
	require.Equal(Fatal, s.inputs[inputPublished.OutPoint()].state)

	// We expect the publish failed input to be markd as failed.
	require.Equal(Fatal, s.inputs[inputPublishFailed.OutPoint()].state)

	// We expect the swept input to stay unchanged.
	require.Equal(Swept, s.inputs[inputSwept.OutPoint()].state)

	// We expect the excluded input to stay unchanged.
	require.Equal(Excluded, s.inputs[inputExcluded.OutPoint()].state)

	// We expect the failed input to stay unchanged.
	require.Equal(Fatal, s.inputs[inputFatal.OutPoint()].state)
}

// TestHandleBumpEventTxFatal checks that `handleBumpEventTxFatal` correctly
// handles a `TxFatal` event.
func TestHandleBumpEventTxFatal(t *testing.T) {
	t.Parallel()

	rt := require.New(t)

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock input set. We are not testing `markInputFailed` here,
	// so the actual set doesn't matter.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)
	set.On("Inputs").Return(nil)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a dummy tx.
	tx := &wire.MsgTx{
		LockTime: 1,
	}

	// Create a testing bump response.
	result := &BumpResult{
		Err: errDummy,
		Tx:  tx,
	}
	resp := &bumpResp{
		result: result,
		set:    set,
	}

	// Mock the store to return an error.
	store.On("DeleteTx", mock.Anything).Return(errDummy).Once()

	// Call the method under test and assert the error is returned.
	err := s.handleBumpEventTxFatal(resp)
	rt.ErrorIs(err, errDummy)

	// Mock the store to return nil.
	store.On("DeleteTx", mock.Anything).Return(nil).Once()

	// Call the method under test and assert no error is returned.
	err = s.handleBumpEventTxFatal(resp)
	rt.NoError(err)
}

// TestHandleUnknownSpendTxOurs checks that `handleUnknownSpendTx` correctly
// marks an input as swept given the tx is ours.
func TestHandleUnknownSpendTxOurs(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PublishFailed)
	op := inp.OutPoint()

	si, ok := s.inputs[op]
	require.True(t, ok)

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}
	txid := tx.TxHash()

	// Mock the store to return true when calling IsOurTx.
	store.On("IsOurTx", txid).Return(true).Once()

	// Call the method under test.
	s.handleUnknownSpendTx(si, tx)

	// Assert the state of the input is updated.
	require.Equal(t, Swept, s.inputs[op].state)
}

// TestHandleUnknownSpendTxThirdParty checks that `handleUnknownSpendTx`
// correctly marks an input as fatal given the tx is not ours.
func TestHandleInputSpendTxThirdParty(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PublishFailed)
	op := inp.OutPoint()

	si, ok := s.inputs[op]
	require.True(t, ok)

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}
	txid := tx.TxHash()

	// Mock the store to return false when calling IsOurTx.
	store.On("IsOurTx", txid).Return(false).Once()

	// Mock `ListSweeps` to return an empty slice as we are testing the
	// workflow here, not the method `removeConflictSweepDescendants`.
	store.On("ListSweeps").Return([]chainhash.Hash{}, nil).Once()

	// Call the method under test.
	s.handleUnknownSpendTx(si, tx)

	// Assert the state of the input is updated.
	require.Equal(t, Fatal, s.inputs[op].state)
}

// TestHandleBumpEventTxUnknownSpendNoRetry checks the case when all the inputs
// are failed due to them being spent by another party.
func TestHandleBumpEventTxUnknownSpendNoRetry(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create a test sweeper.
	s := New(&UtxoSweeperConfig{
		Store: store,
	})

	// Create a mock input.
	inp := createMockInput(t, s, PendingPublish)
	set.On("Inputs").Return([]input.Input{inp})

	op := inp.OutPoint()

	// Create a testing tx that spends the input.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op},
		},
	}
	txid := tx.TxHash()

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxUnknownSpend,
		SpentInputs: map[wire.OutPoint]*wire.MsgTx{
			op: tx,
		},
	}

	// Create a testing bump response.
	resp := &bumpResp{
		result: br,
		set:    set,
	}

	// Mock the store to return true when calling IsOurTx.
	store.On("IsOurTx", txid).Return(true).Once()

	// Call the method under test.
	s.handleBumpEventTxUnknownSpend(resp)

	// Assert the state of the input is updated.
	require.Equal(t, Swept, s.inputs[op].state)
}

// TestHandleBumpEventTxUnknownSpendWithRetry checks the case when some the
// inputs are retried after the bad inputs are filtered out.
func TestHandleBumpEventTxUnknownSpendWithRetry(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &MockSweeperStore{}
	defer store.AssertExpectations(t)

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
		GenSweepScript: func() fn.Result[lnwallet.AddrWithKey] {
			//nolint:ll
			return fn.Ok(lnwallet.AddrWithKey{
				DeliveryAddress: testPubKey.SerializeCompressed(),
			})
		},
		NoDeadlineConfTarget: uint32(DefaultDeadlineDelta),
		Store:                store,
	})

	// Create a mock input set.
	set := &MockInputSet{}
	defer set.AssertExpectations(t)

	// Create mock inputs - inp1 will be the bad input, and inp2 will be
	// retried.
	inp1 := createMockInput(t, s, PendingPublish)
	inp2 := createMockInput(t, s, PendingPublish)
	set.On("Inputs").Return([]input.Input{inp1, inp2})

	op1 := inp1.OutPoint()
	op2 := inp2.OutPoint()

	inp2.On("RequiredLockTime").Return(
		uint32(s.currentHeight), false).Once()
	inp2.On("BlocksToMaturity").Return(uint32(0)).Once()
	inp2.On("HeightHint").Return(uint32(s.currentHeight)).Once()

	// Create a testing tx that spends inp1.
	tx := &wire.MsgTx{
		LockTime: 1,
		TxIn: []*wire.TxIn{
			{PreviousOutPoint: op1},
		},
	}
	txid := tx.TxHash()

	// Create a testing bump result.
	br := &BumpResult{
		Tx:    tx,
		Event: TxUnknownSpend,
		SpentInputs: map[wire.OutPoint]*wire.MsgTx{
			op1: tx,
		},
	}

	// Create a testing bump response.
	resp := &bumpResp{
		result: br,
		set:    set,
	}

	// Mock the store to return true when calling IsOurTx.
	store.On("IsOurTx", txid).Return(true).Once()

	// Mock the aggregator to return an empty slice as we are not testing
	// the actual sweeping behavior.
	aggregator.On("ClusterInputs", mock.Anything).Return([]InputSet{})

	// Call the method under test.
	s.handleBumpEventTxUnknownSpend(resp)

	// Assert the first input is removed.
	require.NotContains(t, s.inputs, op1)

	// Assert the state of the input is updated.
	require.Equal(t, PublishFailed, s.inputs[op2].state)
}
