package sweep

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainio"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// preSignedTxCounter makes every test tx unique.
var preSignedTxCounter atomic.Uint32

// preSignedTestHarness bundles a sweeper built on mocks with the pieces the
// pre-signed tx tests poke at.
type preSignedTestHarness struct {
	s        *UtxoSweeper
	wallet   *MockWallet
	notifier *chainntnfs.MockChainNotifier
	store    *MockSweeperStore
}

// newPreSignedTestHarness creates a sweeper whose collector is NOT running,
// so the tests drive its handlers synchronously, the way the rest of the
// sweeper unit tests do.
func newPreSignedTestHarness(t *testing.T) *preSignedTestHarness {
	t.Helper()

	wallet := &MockWallet{}
	notifier := &chainntnfs.MockChainNotifier{}
	store := NewMockSweeperStore()
	aggregator := &mockUtxoAggregator{}
	estimator := &chainfee.MockEstimator{}

	t.Cleanup(func() {
		wallet.AssertExpectations(t)
		notifier.AssertExpectations(t)
		store.AssertExpectations(t)
		aggregator.AssertExpectations(t)
	})

	// The pre-signed lifecycle never clusters or publishes sweeps itself,
	// but the collector's block handling does, so give it an aggregator
	// that produces no sets.
	aggregator.On("ClusterInputs", mock.Anything).Return(
		[]InputSet{},
	).Maybe()
	estimator.On("RelayFeePerKW").Return(
		chainfee.FeePerKwFloor,
	).Maybe()

	s := New(&UtxoSweeperConfig{
		Wallet:               wallet,
		Notifier:             notifier,
		Store:                store,
		Aggregator:           aggregator,
		FeeEstimator:         estimator,
		NoDeadlineConfTarget: 100,
	})
	s.currentHeight = 100

	return &preSignedTestHarness{
		s:        s,
		wallet:   wallet,
		notifier: notifier,
		store:    store,
	}
}

// newPreSignedTx builds a two-output pre-signed tx (HTLC output + anchor)
// with the given locktime and the anchor input spending its index 1.
func newPreSignedTx(t *testing.T, lockTime uint32) (*wire.MsgTx, input.Input) {
	t.Helper()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Give every tx a distinct prevout so two txs built in the same test
	// never collide on txid.
	tx := wire.NewMsgTx(2)
	tx.LockTime = lockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Index: preSignedTxCounter.Add(1),
		},
	})
	tx.AddTxOut(&wire.TxOut{Value: 630, PkScript: []byte{0x51}})
	tx.AddTxOut(&wire.TxOut{Value: 330, PkScript: []byte{0x52}})

	op := wire.OutPoint{Hash: tx.TxHash(), Index: 1}
	signDesc := &input.SignDescriptor{
		KeyDesc:  keychain.KeyDescriptor{PubKey: priv.PubKey()},
		Output:   tx.TxOut[1],
		HashType: txscript.SigHashDefault,
	}
	anchor := input.MakeBaseInput(
		&op, input.TaprootAnchorSweepSpend, signDesc, 100,
		&input.TxInfo{Fee: 227, Weight: 817},
	)

	return tx, &anchor
}

// expectSpendRegistration mocks a successful spend registration for the
// given outpoint and returns the channel the test can deliver the spend on.
func (h *preSignedTestHarness) expectSpendRegistration(
	op wire.OutPoint) chan *chainntnfs.SpendDetail {

	spendChan := make(chan *chainntnfs.SpendDetail, 1)
	h.notifier.On(
		"RegisterSpendNtfn", &op, mock.Anything, mock.Anything,
	).Return(&chainntnfs.SpendEvent{
		Spend:  spendChan,
		Cancel: func() {},
	}, nil).Once()

	return spendChan
}

// submit hands a request to the collector handler directly and returns the
// listener channel.
func (h *preSignedTestHarness) submit(t *testing.T,
	req PreSignedTxRequest) chan Result {

	t.Helper()

	resultChan := make(chan Result, 1)
	h.s.handlePreSignedTxReq(&preSignedTxMessage{
		req:        req,
		resultChan: resultChan,
	})

	return resultChan
}

// nextBlock mimics what the collector does on a new block: prune final-state
// inputs, then drive the pre-signed txs.
func (h *preSignedTestHarness) nextBlock() {
	h.s.currentHeight++
	h.s.updateSweeperInputs()
	h.s.processPreSignedTxs()
}

// requireNoResult asserts that no final result has been delivered.
func requireNoResult(t *testing.T, resultChan <-chan Result) {
	t.Helper()

	select {
	case r := <-resultChan:
		t.Fatalf("unexpected result: %v", r)
	default:
	}
}

// requireResult waits for the final result of a lifecycle.
func requireResult(t *testing.T, resultChan <-chan Result) Result {
	t.Helper()

	select {
	case r := <-resultChan:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no result delivered")
	}

	return Result{}
}

// TestPublishPreSignedTxValidation asserts the synchronous validation of a
// request: a tx is required, and an anchor, when given, must be a well formed
// input that actually belongs to the tx.
func TestPublishPreSignedTxValidation(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)

	_, err := h.s.PublishPreSignedTx(PreSignedTxRequest{})
	require.ErrorContains(t, err, "nil pre-signed tx")

	emptyAnchor := &input.MockInput{}
	emptyAnchor.On("OutPoint").Return(input.EmptyOutPoint)
	_, err = h.s.PublishPreSignedTx(PreSignedTxRequest{
		Tx: tx, Anchor: emptyAnchor,
	})
	require.ErrorContains(t, err, "invalid anchor input")

	otherTx, otherAnchor := newPreSignedTx(t, 0)
	require.NotEqual(t, tx.TxHash(), otherTx.TxHash())
	_, err = h.s.PublishPreSignedTx(PreSignedTxRequest{
		Tx: tx, Anchor: otherAnchor,
	})
	require.ErrorContains(t, err, "does not belong to tx")

	// A valid request against a stopped sweeper (collector not running,
	// quit closed) is refused instead of blocking.
	close(h.s.quit)
	_, err = h.s.PublishPreSignedTx(PreSignedTxRequest{
		Tx: tx, Anchor: anchor,
	})
	require.ErrorIs(t, err, ErrSweeperShuttingDown)
}

// TestPreSignedTxLocktimeGate asserts that a locktime'd tx is not published
// before the chain reaches its locktime, and is published (and its anchor
// registered) on the first block at which it is final.
func TestPreSignedTxLocktimeGate(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 103)
	deadline := fn.Some(int32(200))

	resultChan := h.submit(t, PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor, Budget: 10_000,
		DeadlineHeight: deadline,
	})

	// Heights 100, 101 and 102 are below the locktime: nothing may be
	// published and the wallet mock has no expectation set.
	st := h.s.preSigned[tx.TxHash()]
	require.NotNil(t, st)
	require.False(t, st.published)
	h.nextBlock()
	h.nextBlock()
	require.False(t, st.published)
	requireNoResult(t, resultChan)

	// Height 103: the tx is final, published, and its anchor is handed
	// to the regular sweep machinery with our budget and deadline.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()

	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	pi, ok := h.s.inputs[anchor.OutPoint()]
	require.True(t, ok, "anchor must be registered as a sweeper input")
	require.Equal(t, btcutil.Amount(10_000), pi.params.Budget)
	require.Equal(t, deadline, pi.params.DeadlineHeight)
	require.EqualValues(t, 200, pi.DeadlineHeight)
	requireNoResult(t, resultChan)
}

// TestPreSignedTxPublishRetry asserts that a failed publication is retried
// on the next block (not immediately, not never), and that once it succeeds
// the lifecycle moves on to the anchor.
func TestPreSignedTxPublishRetry(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)

	// The initial attempt (made directly on submission) fails.
	h.wallet.On("PublishTransaction", tx, "test").Return(
		errors.New("mempool min fee not met"),
	).Once()
	resultChan := h.submit(t, PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor,
		DeadlineHeight: fn.Some(int32(200)),
	})
	st := h.s.preSigned[tx.TxHash()]
	require.False(t, st.published)
	requireNoResult(t, resultChan)

	// The next block retries and succeeds.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()

	require.True(t, st.published)
	require.True(t, st.anchorOffered)

	// Further blocks must not re-publish or re-register (the mocks are
	// .Once(), so a second call would fail the test).
	h.nextBlock()
	h.nextBlock()
	requireNoResult(t, resultChan)
}

// TestPreSignedTxPublishDeadline asserts that publication failures are
// retried only up to the deadline height: past it the lifecycle ends with
// ErrPreSignedTxDeadline instead of retrying forever.
func TestPreSignedTxPublishDeadline(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	publishErr := errors.New("rejected")

	// Submission at height 100 with a deadline at 102: attempts at 100,
	// 101 and 102 are retries, the attempt at 103 is past the deadline.
	h.wallet.On("PublishTransaction", tx, "test").Return(publishErr).
		Times(4)
	resultChan := h.submit(t, PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor,
		DeadlineHeight: fn.Some(int32(102)),
	})

	h.nextBlock() // 101
	h.nextBlock() // 102
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())

	h.nextBlock() // 103: deadline passed, give up.
	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrPreSignedTxDeadline)
	require.ErrorIs(t, result.Err, publishErr)
	require.NotContains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxNoDeadlineKeepsRetrying asserts that without a deadline a
// failing publication is retried indefinitely rather than expiring.
func TestPreSignedTxNoDeadlineKeepsRetrying(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 0)

	h.wallet.On("PublishTransaction", tx, "test").Return(
		errors.New("rejected"),
	).Times(6)
	resultChan := h.submit(t, PreSignedTxRequest{Tx: tx, Label: "test"})

	for range 5 {
		h.nextBlock()
	}
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxAnchorRegistrationRetry asserts the registration
// acknowledgement boundary: when the anchor cannot be registered (spend
// notification registration fails), the lifecycle does NOT consider the
// anchor offered, and retries the registration on the next block.
func TestPreSignedTxAnchorRegistrationRetry(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.notifier.On(
		"RegisterSpendNtfn", &op, mock.Anything, mock.Anything,
	).Return(nil, errors.New("notifier down")).Once()

	resultChan := h.submit(t, PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor,
		DeadlineHeight: fn.Some(int32(200)),
	})
	st := h.s.preSigned[tx.TxHash()]
	require.True(t, st.published, "publication must not be redone")
	require.False(t, st.anchorOffered,
		"a failed registration is not an acknowledgement")
	requireNoResult(t, resultChan)

	// Next block: the publication is not repeated (mock is .Once()), the
	// registration is retried and now succeeds.
	h.expectSpendRegistration(op)
	h.nextBlock()
	require.True(t, st.anchorOffered)
	require.Contains(t, h.s.inputs, op)
	require.Equal(t, Init, h.s.inputs[op].state)
}

// TestPreSignedTxAnchorRegistrationDeadline asserts that registration
// failures also stop at the deadline.
func TestPreSignedTxAnchorRegistrationDeadline(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.notifier.On(
		"RegisterSpendNtfn", &op, mock.Anything, mock.Anything,
	).Return(nil, errors.New("notifier down")).Times(2)

	resultChan := h.submit(t, PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor,
		DeadlineHeight: fn.Some(int32(100)),
	})
	requireNoResult(t, resultChan)

	h.nextBlock() // 101: past the deadline.
	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrPreSignedTxDeadline)
	require.NotContains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxWithoutAnchor asserts that a tx without an anchor concludes
// successfully as soon as it is published, with the tx itself as the result.
func TestPreSignedTxWithoutAnchor(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 0)

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	resultChan := h.submit(t, PreSignedTxRequest{Tx: tx, Label: "test"})

	result := requireResult(t, resultChan)
	require.NoError(t, result.Err)
	require.Equal(t, tx.TxHash(), result.Tx.TxHash())
	require.NotContains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxDuplicateRequest asserts that re-submitting an in-flight tx
// (resolver relaunch) attaches an additional listener instead of restarting
// the lifecycle, and that every listener receives the final result.
func TestPreSignedTxDuplicateRequest(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())

	req := PreSignedTxRequest{Tx: tx, Label: "test", Anchor: anchor}
	first := h.submit(t, req)
	second := h.submit(t, req)

	st := h.s.preSigned[tx.TxHash()]
	require.Len(t, st.listeners, 2)

	// Conclude via the anchor result path.
	child := wire.NewMsgTx(2)
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Tx: child},
	})

	for _, c := range []chan Result{first, second} {
		result := requireResult(t, c)
		require.NoError(t, result.Err)
		require.Equal(t, child.TxHash(), result.Tx.TxHash())
	}
	require.NotContains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxAnchorOutcomes asserts how the anchor sweep's final result
// concludes the lifecycle: every outcome the sweeper reports is final (the
// sweeper already retried internally), whether success, a third-party spend
// of the anchor, or a terminal failure.
func TestPreSignedTxAnchorOutcomes(t *testing.T) {
	t.Parallel()

	child := wire.NewMsgTx(2)
	child.AddTxOut(&wire.TxOut{Value: 1})

	testCases := []struct {
		name   string
		result Result
	}{{
		name:   "swept by our child",
		result: Result{Tx: child},
	}, {
		name:   "remote spend of the anchor",
		result: Result{Err: ErrRemoteSpend},
	}, {
		name:   "terminal sweep failure",
		result: Result{Err: ErrNotEnoughBudget},
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newPreSignedTestHarness(t)
			tx, anchor := newPreSignedTx(t, 0)

			h.wallet.On("PublishTransaction", tx, "test").
				Return(nil).Once()
			h.expectSpendRegistration(anchor.OutPoint())
			resultChan := h.submit(t, PreSignedTxRequest{
				Tx: tx, Label: "test", Anchor: anchor,
			})
			requireNoResult(t, resultChan)

			h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
				txid:   tx.TxHash(),
				result: tc.result,
			})

			result := requireResult(t, resultChan)
			require.Equal(t, tc.result, result)
			require.NotContains(t, h.s.preSigned, tx.TxHash())

			// No re-offer must follow a final result.
			h.nextBlock()
			requireNoResult(t, resultChan)
		})
	}
}

// TestPreSignedTxUnknownAnchorResult asserts that a late anchor result for a
// lifecycle that already concluded is ignored.
func TestPreSignedTxUnknownAnchorResult(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   chainhash.Hash{1},
		result: Result{Err: ErrRemoteSpend},
	})
	require.Empty(t, h.s.preSigned)
}

// TestPreSignedTxShutdown asserts that pending lifecycles are released with
// ErrSweeperShuttingDown when the sweeper stops.
func TestPreSignedTxShutdown(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 500)

	resultChan := h.submit(t, PreSignedTxRequest{Tx: tx, Label: "test"})
	requireNoResult(t, resultChan)

	h.s.failPreSignedTxs(ErrSweeperShuttingDown)

	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrSweeperShuttingDown)
	require.Empty(t, h.s.preSigned)
}

// TestPreSignedTxEndToEnd runs the lifecycle through the live collector: a
// request is submitted via the public API, a block is processed, the tx is
// published, the anchor registered, and a spend of the anchor by our own
// sweep tx concludes the lifecycle with that tx as the result.
func TestPreSignedTxEndToEnd(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 101)
	op := anchor.OutPoint()

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	spendChan := h.expectSpendRegistration(op)

	child := wire.NewMsgTx(2)
	child.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	childHash := child.TxHash()
	h.store.On("IsOurTx", childHash).Return(true).Once()

	beat := &chainio.MockBlockbeat{}
	beat.On("Height").Return(int32(100)).Maybe()
	beat.On("logger").Return(log).Maybe()
	require.NoError(t, h.s.Start(beat))
	t.Cleanup(func() {
		require.NoError(t, h.s.Stop())
	})

	resultChan, err := h.s.PublishPreSignedTx(PreSignedTxRequest{
		Tx: tx, Label: "test", Anchor: anchor, Budget: 10_000,
		DeadlineHeight: fn.Some(int32(200)),
	})
	require.NoError(t, err)

	// Height 100 is below the locktime; the collector must hold the tx.
	requireNoResult(t, resultChan)

	// Process block 101 through the real blockbeat path: publish and
	// anchor registration happen inside the collector.
	beat101 := &chainio.MockBlockbeat{}
	beat101.On("Height").Return(int32(101)).Maybe()
	beat101.On("logger").Return(log).Maybe()
	require.NoError(t, h.s.ProcessBlock(beat101))

	// Deliver the spend of the anchor by our child tx. The sweeper's
	// regular spend handling marks the anchor swept and the lifecycle
	// concludes with the spending tx.
	spendChan <- &chainntnfs.SpendDetail{
		SpentOutPoint: &op,
		SpendingTx:    child,
		SpenderTxHash: &childHash,
	}

	result := requireResult(t, resultChan)
	require.NoError(t, result.Err)
	require.Equal(t, childHash, result.Tx.TxHash())
}
