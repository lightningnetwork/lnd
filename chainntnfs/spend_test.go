package chainntnfs_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var filterScript = append([]byte{0x00, 0x14}, make([]byte, 20)...)

type spendWaitResult struct {
	spend *chainntnfs.SpendDetail
	err   error
}

type captureNotifier struct {
	chainntnfs.MockChainNotifier
	registrations chan spendConfirmationRegistration
}

type spendConfirmationRegistration struct {
	pkScript []byte
	txIDOnly bool
}

// RegisterConfirmationsNtfn captures notifier options before delegating to
// the test mock.
func (s *captureNotifier) RegisterConfirmationsNtfn(
	txid *chainhash.Hash, pkScript []byte, numConfs, heightHint uint32,
	opts ...chainntnfs.NotifierOption) (
	*chainntnfs.ConfirmationEvent, error) {

	options := chainntnfs.DefaultNotifierOptions()
	for _, option := range opts {
		option(options)
	}
	s.registrations <- spendConfirmationRegistration{
		pkScript: pkScript, txIDOnly: options.TxIDOnlyMatch,
	}

	return s.MockChainNotifier.RegisterConfirmationsNtfn(
		txid, pkScript, numConfs, heightHint, opts...,
	)
}

type confirmationHarness struct {
	event       *chainntnfs.ConfirmationEvent
	registered  <-chan struct{}
	canceled    <-chan struct{}
	cancelCount *atomic.Int32
}

// newSpend creates a candidate with an unfilterable first output.
func newSpend(n byte, height int32) *chainntnfs.SpendDetail {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: uint32(n)},
	})
	tx.AddTxOut(&wire.TxOut{
		Value: int64(n), PkScript: []byte{txscript.OP_RETURN, n},
	})
	hash := tx.TxHash()

	return &chainntnfs.SpendDetail{
		SpenderTxHash: &hash, SpendingTx: tx, SpendingHeight: height,
	}
}

// newConfirmation prepares one expected confirmation registration.
func newConfirmation(spend *chainntnfs.SpendDetail,
	notifier *captureNotifier,
	numConfs uint32) confirmationHarness {

	registered := make(chan struct{})
	canceled := make(chan struct{})
	cancelCount := &atomic.Int32{}
	confEvent := chainntnfs.NewConfirmationEvent(numConfs, func() {
		cancelCount.Add(1)
		close(canceled)
	})
	notifier.On(
		"RegisterConfirmationsNtfn", spend.SpenderTxHash,
		filterScript, numConfs, uint32(spend.SpendingHeight),
	).Run(func(_ mock.Arguments) {
		close(registered)
	}).Return(confEvent, nil).Once()

	return confirmationHarness{
		event: confEvent, registered: registered, canceled: canceled,
		cancelCount: cancelCount,
	}
}

// receiveSpendResult waits for the subject goroutine to return.
func receiveSpendResult(t *testing.T,
	results <-chan spendWaitResult) spendWaitResult {

	t.Helper()
	select {
	case result := <-results:
		return result
	case <-time.After(time.Second):
		t.Fatal("spend wait did not return")
		return spendWaitResult{}
	}
}

// waitForSignal waits for a deterministic transition signal.
func waitForSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal("expected signal not received")
	}
}

// TestWaitForSpendConfirmationsTransitions verifies candidate changes cannot
// complete with an obsolete spend.
func TestWaitForSpendConfirmationsTransitions(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name       string
		action     string
		staleReorg bool
	}{
		{name: "negative", action: "negative", staleReorg: true},
		{name: "spend reorg", action: "reorg"},
		{name: "replacement", action: "replacement"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			first := newSpend(1, 10)
			replacement := newSpend(2, 11)
			notifier := &captureNotifier{registrations: make(
				chan spendConfirmationRegistration, 2,
			)}
			firstConf := newConfirmation(first, notifier, 3)
			nextConf := newConfirmation(replacement, notifier, 3)
			finality, err := chainntnfs.NewSpendFinality(3)
			require.NoError(t, err)
			event := chainntnfs.NewSpendEvent(func() {})
			results := make(chan spendWaitResult, 1)
			go func() {
				result := spendWaitResult{}
				result.spend, result.err =
					chainntnfs.WaitForSpendConfirmations(
						event, notifier, filterScript,
						finality, make(chan struct{}),
					)
				results <- result
			}()

			event.Spend <- first
			waitForSignal(t, firstConf.registered)
			registration := <-notifier.registrations
			require.Equal(t, filterScript, registration.pkScript)
			require.True(t, registration.txIDOnly)
			switch testCase.action {
			case "negative":
				firstConf.event.NegativeConf <- 1
			case "reorg":
				event.Reorg <- struct{}{}
			case "replacement":
				event.Spend <- replacement
			}
			waitForSignal(t, firstConf.canceled)
			select {
			case <-results:
				t.Fatal("obsolete candidate returned")
			default:
			}
			if testCase.action != "replacement" {
				if testCase.staleReorg {
					event.Reorg <- struct{}{}
				}
				event.Spend <- replacement
			}
			waitForSignal(t, nextConf.registered)
			registration = <-notifier.registrations
			require.Equal(t, filterScript, registration.pkScript)
			require.True(t, registration.txIDOnly)
			nextConf.event.Confirmed <- &chainntnfs.TxConfirmation{}

			result := receiveSpendResult(t, results)
			require.NoError(t, result.err)
			require.Same(t, replacement, result.spend)
			require.Equal(t, int32(1), firstConf.cancelCount.Load())
			require.Equal(t, int32(1), nextConf.cancelCount.Load())
			notifier.AssertExpectations(t)
		})
	}
}

// TestWaitForSpendConfirmationsBufferedReorg verifies a queued reorg cannot
// make a depth-one candidate final without authoritative confirmation.
func TestWaitForSpendConfirmationsBufferedReorg(t *testing.T) {
	t.Parallel()

	first := newSpend(3, 12)
	notifier := &captureNotifier{
		registrations: make(chan spendConfirmationRegistration, 1),
	}
	firstConf := newConfirmation(first, notifier, 1)
	finality, err := chainntnfs.NewSpendFinality(1)
	require.NoError(t, err)
	event := chainntnfs.NewSpendEvent(func() {})
	event.Spend <- first
	event.Reorg <- struct{}{}
	results := make(chan spendWaitResult, 1)
	go func() {
		spend, err := chainntnfs.WaitForSpendConfirmations(
			event, notifier, filterScript, finality,
			make(chan struct{}),
		)
		results <- spendWaitResult{spend: spend, err: err}
	}()

	waitForSignal(t, firstConf.registered)
	registration := <-notifier.registrations
	require.Equal(t, filterScript, registration.pkScript)
	require.True(t, registration.txIDOnly)
	select {
	case <-results:
		t.Fatal("reorged depth-one candidate returned")
	default:
	}
	firstConf.event.Confirmed <- &chainntnfs.TxConfirmation{
		BlockHeight: 13,
		Tx:          first.SpendingTx,
	}

	result := receiveSpendResult(t, results)
	require.NoError(t, result.err)
	require.NotSame(t, first, result.spend)
	require.Equal(t, int32(13), result.spend.SpendingHeight)
	require.Equal(t, first.SpenderTxHash, result.spend.SpenderTxHash)
	require.Equal(t, int32(1), firstConf.cancelCount.Load())
	notifier.AssertExpectations(t)
}

// TestWaitForSpendConfirmationsShutdown verifies closed spend and quit paths.
func TestWaitForSpendConfirmationsShutdown(t *testing.T) {
	t.Parallel()

	for _, closeSpend := range []bool{true, false} {
		event := chainntnfs.NewSpendEvent(func() {})
		quit := make(chan struct{})
		finality, err := chainntnfs.NewSpendFinality(2)
		require.NoError(t, err)
		results := make(chan spendWaitResult, 1)
		go func() {
			spend, err := chainntnfs.WaitForSpendConfirmations(
				event, &chainntnfs.MockChainNotifier{},
				filterScript, finality, quit,
			)
			results <- spendWaitResult{spend: spend, err: err}
		}()
		if closeSpend {
			close(event.Spend)
		} else {
			close(quit)
		}

		result := receiveSpendResult(t, results)
		require.ErrorIs(
			t, result.err, chainntnfs.ErrChainNotifierShuttingDown,
		)
	}
}

// TestWaitForSpendConfirmationsInvalidFinality verifies early validation
// still cancels the spend subscription.
func TestWaitForSpendConfirmationsInvalidFinality(t *testing.T) {
	t.Parallel()

	cancelCount := &atomic.Int32{}
	event := chainntnfs.NewSpendEvent(func() {
		cancelCount.Add(1)
	})
	spend, err := chainntnfs.WaitForSpendConfirmations(
		event, &chainntnfs.MockChainNotifier{}, filterScript, nil,
		make(chan struct{}),
	)
	require.ErrorIs(t, err, chainntnfs.ErrNumConfsOutOfRange)
	require.Nil(t, spend)
	require.Equal(t, int32(1), cancelCount.Load())
}
