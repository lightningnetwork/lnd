package sweep

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

func missingInputTestRecord(inputs ...input.Input) (*monitorRecord,
	*MockFeeFunction) {

	feeRate := chainfee.SatPerKWeight(1_000)
	feeFunc := &MockFeeFunction{}
	feeFunc.On("FeeRate").Return(feeRate).Once()
	feeFunc.On("Increment").Return(true, nil).Once()

	return &monitorRecord{
		requestID: 1,
		tx:        &wire.MsgTx{},
		req: &BumpRequest{
			Inputs: inputs,
		},
		feeFunction: feeFunc,
	}, feeFunc
}

func emptySpendNotifier(t *testing.T, calls int) *chainntnfs.MockChainNotifier {
	t.Helper()

	notifier := &chainntnfs.MockChainNotifier{}
	notifier.On(
		"RegisterSpendNtfn", mock.Anything, mock.Anything, uint32(1),
	).Return(chainntnfs.NewSpendEvent(func() {}), nil).Times(calls)

	t.Cleanup(func() {
		notifier.AssertExpectations(t)
	})

	return notifier
}

// TestHandleMissingInputsRetryBranches verifies that ambiguous lookup outcomes
// remain retryable instead of turning the whole batch fatal.
func TestHandleMissingInputsRetryBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		lookup   func(input.Input) (bool, error)
		expected error
	}{
		{
			name: "all inputs remain unspent",
			lookup: func(input.Input) (bool, error) {
				return true, nil
			},
			expected: ErrInputMissing,
		},
		{
			name: "blocking lookup fails",
			lookup: func(input.Input) (bool, error) {
				return false, errDummy
			},
			expected: errDummy,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			inp := createTestInput(10_000, input.WitnessKeyHash)
			record, feeFunc := missingInputTestRecord(&inp)
			defer feeFunc.AssertExpectations(t)

			publisher := NewTxPublisher(TxPublisherConfig{
				Notifier:       emptySpendNotifier(t, 1),
				IsInputUnspent: testCase.lookup,
			})

			result := publisher.handleMissingInputs(record)

			require.Equal(t, TxFailed, result.Event)
			require.ErrorIs(t, result.Err, testCase.expected)
			require.Empty(t, result.MissingInputs)
		})
	}
}

// TestHandleMissingInputsNilLookup preserves the compatibility fallback for
// callers that do not provide the blocking lookup callback.
func TestHandleMissingInputsNilLookup(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	publisher := NewTxPublisher(TxPublisherConfig{
		Notifier: emptySpendNotifier(t, 1),
	})
	record := &monitorRecord{
		requestID: 1,
		tx:        &wire.MsgTx{},
		req: &BumpRequest{
			Inputs: []input.Input{&inp},
		},
	}

	result := publisher.handleMissingInputs(record)

	require.Equal(t, TxFatal, result.Event)
	require.ErrorIs(t, result.Err, ErrInputMissing)
}

// TestHandleMissingInputsWalletParent verifies that a parent transaction known
// to the wallet keeps an unconfirmed output retryable even when the chain UTXO
// lookup excludes mempool outputs.
func TestHandleMissingInputsWalletParent(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	op := inp.OutPoint()
	parent := wire.NewMsgTx(2)
	parent.AddTxOut(&wire.TxOut{Value: 10_000})

	wallet := &MockWallet{}
	wallet.On("FetchTx", op.Hash).Return(parent, nil).Once()
	wallet.On("GetTransactionDetails", mock.Anything).Return(
		&lnwallet.TransactionDetail{NumConfirmations: 0}, nil,
	).Once()
	defer wallet.AssertExpectations(t)

	record, feeFunc := missingInputTestRecord(&inp)
	defer feeFunc.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Wallet:   wallet,
		Notifier: emptySpendNotifier(t, 1),
		IsInputUnspent: func(input.Input) (bool, error) {
			return false, nil
		},
	})

	result := publisher.handleMissingInputs(record)

	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, ErrInputMissing)
	require.Empty(t, result.MissingInputs)
}

// TestFindMissingInputsConfirmedWalletParent verifies that a wallet-known
// parent alone does not keep a confirmed, spent output retryable.
func TestFindMissingInputsConfirmedWalletParent(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	op := inp.OutPoint()
	parent := wire.NewMsgTx(2)
	parent.AddTxOut(&wire.TxOut{Value: 10_000})

	wallet := &MockWallet{}
	wallet.On("FetchTx", op.Hash).Return(parent, nil).Once()
	wallet.On("GetTransactionDetails", mock.Anything).Return(
		&lnwallet.TransactionDetail{NumConfirmations: 1}, nil,
	).Once()
	defer wallet.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Wallet: wallet,
		IsInputUnspent: func(input.Input) (bool, error) {
			return false, nil
		},
	})

	missing, err := publisher.findMissingInputs([]input.Input{&inp})

	require.NoError(t, err)
	require.Contains(t, missing, op)
}

// TestHandleMissingInputsMempoolSpend verifies that a spender already visible
// in the mempool is treated as an unknown spend even if the notifier has not
// delivered its historical spend notification yet.
func TestHandleMissingInputsMempoolSpend(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	op := inp.OutPoint()
	spendingTx := wire.MsgTx{Version: 2}

	mempool := &chainntnfs.MockMempoolWatcher{}
	mempool.On("LookupInputMempoolSpend", op).
		Return(fn.Some(spendingTx)).Once()
	defer mempool.AssertExpectations(t)

	record, feeFunc := missingInputTestRecord(&inp)
	defer feeFunc.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Notifier: emptySpendNotifier(t, 1),
		Mempool:  mempool,
	})

	result := publisher.handleMissingInputs(record)

	require.Equal(t, TxUnknownSpend, result.Event)
	require.ErrorIs(t, result.Err, ErrUnknownSpent)
	require.Contains(t, result.SpentInputs, op)
	require.Equal(t, spendingTx.TxHash(), result.SpentInputs[op].TxHash())
}

// TestFindMissingInputsWalletLookupError verifies that a wallet fallback error
// is surfaced so the caller can retry rather than classifying an input.
func TestFindMissingInputsWalletLookupError(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	op := inp.OutPoint()

	wallet := &MockWallet{}
	wallet.On("FetchTx", op.Hash).Return(nil, errDummy).Once()
	defer wallet.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Wallet: wallet,
		IsInputUnspent: func(input.Input) (bool, error) {
			return false, nil
		},
	})

	missing, err := publisher.findMissingInputs([]input.Input{&inp})

	require.Nil(t, missing)
	require.True(t, errors.Is(err, errDummy))
}

// TestHandleInitialTxErrorMissingInput verifies the initial error path routes
// ErrInputMissing through the missing-input classifier.
func TestHandleInitialTxErrorMissingInput(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	record, feeFunc := missingInputTestRecord(&inp)
	defer feeFunc.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Notifier: emptySpendNotifier(t, 1),
		IsInputUnspent: func(input.Input) (bool, error) {
			return true, nil
		},
	})
	resultChan := make(chan *BumpResult, 1)
	publisher.subscriberChans.Store(record.requestID, resultChan)

	publisher.handleInitialTxError(record, ErrInputMissing)

	result := <-resultChan
	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, ErrInputMissing)
}

// TestHandleReplacementTxErrorMissingInput verifies the replacement path also
// routes ErrInputMissing through the missing-input classifier.
func TestHandleReplacementTxErrorMissingInput(t *testing.T) {
	t.Parallel()

	inp := createTestInput(10_000, input.WitnessKeyHash)
	record, feeFunc := missingInputTestRecord(&inp)
	defer feeFunc.AssertExpectations(t)

	publisher := NewTxPublisher(TxPublisherConfig{
		Notifier: emptySpendNotifier(t, 1),
		IsInputUnspent: func(input.Input) (bool, error) {
			return true, nil
		},
	})

	resultOpt := publisher.handleReplacementTxError(
		record, wire.NewMsgTx(2), ErrInputMissing,
	)
	result, err := resultOpt.UnwrapOrErr(errors.New("missing bump result"))

	require.NoError(t, err)
	require.Equal(t, TxFailed, result.Event)
	require.ErrorIs(t, result.Err, ErrInputMissing)
}
