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
	"github.com/lightningnetwork/lnd/lnwallet"
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

// spentOutputs returns the outputs a test tx spends, one per input.
func spentOutputs(tx *wire.MsgTx) []*wire.TxOut {
	outs := make([]*wire.TxOut, len(tx.TxIn))
	for i := range outs {
		outs[i] = &wire.TxOut{Value: 1200, PkScript: []byte{0x53}}
	}

	return outs
}

// newRequest builds a well formed request for the given tx.
func newRequest(tx *wire.MsgTx, anchor input.Input) PreSignedTxRequest {
	return PreSignedTxRequest{
		Tx:           tx,
		SpentOutputs: spentOutputs(tx),
		HeightHint:   90,
		Label:        "test",
		Anchor:       anchor,
	}
}

// spendWatch is a mocked spend registration: the channel the test delivers
// the spend on, and how often the registration was cancelled.
type spendWatch struct {
	spend     chan *chainntnfs.SpendDetail
	cancelled atomic.Int32
}

// expectSpendRegistration mocks a successful spend registration for the
// given outpoint.
func (h *preSignedTestHarness) expectSpendRegistration(
	op wire.OutPoint) *spendWatch {

	w := &spendWatch{
		spend: make(chan *chainntnfs.SpendDetail, 1),
	}
	h.notifier.On(
		"RegisterSpendNtfn", &op, mock.Anything, mock.Anything,
	).Return(&chainntnfs.SpendEvent{
		Spend: w.spend,
		Cancel: func() {
			w.cancelled.Add(1)
		},
	}, nil).Once()

	return w
}

// expectInputWatch mocks the spend registration the lifecycle makes for the
// single input of a test tx, asserting it is made with the spent output's
// script and the request's height hint.
func (h *preSignedTestHarness) expectInputWatch(
	req PreSignedTxRequest) *spendWatch {

	op := req.Tx.TxIn[0].PreviousOutPoint
	w := &spendWatch{
		spend: make(chan *chainntnfs.SpendDetail, 1),
	}
	h.notifier.On(
		"RegisterSpendNtfn", &op, req.SpentOutputs[0].PkScript,
		req.HeightHint,
	).Return(&chainntnfs.SpendEvent{
		Spend: w.spend,
		Cancel: func() {
			w.cancelled.Add(1)
		},
	}, nil).Once()

	return w
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

// spendInput hands the collector a confirmed spend of the input of the given
// tx by the given spender, the way the input watch delivers it.
func (h *preSignedTestHarness) spendInput(tx, spender *wire.MsgTx) {
	op := tx.TxIn[0].PreviousOutPoint
	spenderHash := spender.TxHash()
	h.s.handlePreSignedSpend(&preSignedSpend{
		txid: tx.TxHash(),
		spend: &chainntnfs.SpendDetail{
			SpentOutPoint:  &op,
			SpendingTx:     spender,
			SpenderTxHash:  &spenderHash,
			SpendingHeight: h.s.currentHeight,
		},
	})
}

// conflictingTx builds a tx that spends the same input as the given tx.
func conflictingTx(tx *wire.MsgTx) *wire.MsgTx {
	conflict := wire.NewMsgTx(2)
	conflict.AddTxIn(&wire.TxIn{
		PreviousOutPoint: tx.TxIn[0].PreviousOutPoint,
	})
	conflict.AddTxOut(&wire.TxOut{Value: 1})

	return conflict
}

// failAnchorMissingParent mimics what the sweeper does when the anchor child
// can no longer be published because its parent left the mempool: the anchor
// input is marked fatal and its ErrInputMissing result is handed to the
// lifecycle.
func (h *preSignedTestHarness) failAnchorMissingParent(tx *wire.MsgTx,
	anchor input.Input) {

	h.s.inputs[anchor.OutPoint()].state = Fatal
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Err: ErrInputMissing},
	})
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
// request: a tx is required, every input needs its spent output, and an
// anchor, when given, must be a well formed input that actually belongs to
// the tx. Every rejection wraps ErrInvalidPreSignedTxRequest.
func TestPublishPreSignedTxValidation(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	otherTx, otherAnchor := newPreSignedTx(t, 0)

	emptyAnchor := &input.MockInput{}
	emptyAnchor.On("OutPoint").Return(input.EmptyOutPoint)

	testCases := []struct {
		name string
		req  PreSignedTxRequest
		ok   bool
	}{{
		name: "nil tx",
		req:  PreSignedTxRequest{},
	}, {
		name: "no spent outputs",
		req:  PreSignedTxRequest{Tx: tx},
	}, {
		name: "too many spent outputs",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(otherTx)[:0],
		},
	}, {
		name: "spent output without script",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: []*wire.TxOut{{Value: 1}},
		},
	}, {
		name: "empty anchor",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor:       emptyAnchor,
		},
	}, {
		name: "anchor of another tx",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor:       otherAnchor,
		},
	}, {
		name: "well formed without anchor",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
		},
		ok: true,
	}, {
		name: "well formed with anchor",
		req:  newRequest(tx, anchor),
		ok:   true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePreSignedTxRequest(tc.req)
			if tc.ok {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrInvalidPreSignedTxRequest)

			// The public API rejects the same request before it
			// reaches the collector.
			_, err = h.s.PublishPreSignedTx(tc.req)
			require.ErrorIs(t, err, ErrInvalidPreSignedTxRequest)
		})
	}

	require.Empty(t, h.s.preSigned)
}

// TestPreSignedTxLocktimeGate asserts that a tx whose locktime lies in the
// future is not published until the chain reaches that height, while its
// inputs are watched from the start.
func TestPreSignedTxLocktimeGate(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 102)
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	resultChan := h.submit(t, req)

	st := h.s.preSigned[tx.TxHash()]
	require.True(t, st.watching)
	require.False(t, st.published)
	requireNoResult(t, resultChan)

	// Height 101 is still below the locktime.
	h.nextBlock()
	require.False(t, st.published)

	// Height 102 reaches it: publish and register the anchor.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()

	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	require.Contains(t, h.s.inputs, anchor.OutPoint())
	requireNoResult(t, resultChan)
}

// TestPreSignedTxInputWatchRetry asserts that nothing is published before
// the inputs of the tx are watched: a failed spend registration is retried
// on the next block, and only then does the publication follow.
func TestPreSignedTxInputWatchRetry(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)
	htlcOp := tx.TxIn[0].PreviousOutPoint

	h.notifier.On(
		"RegisterSpendNtfn", &htlcOp, mock.Anything, mock.Anything,
	).Return(nil, errors.New("notifier down")).Once()
	resultChan := h.submit(t, req)

	st := h.s.preSigned[tx.TxHash()]
	require.False(t, st.watching)
	require.False(t, st.published, "must not publish unwatched")
	requireNoResult(t, resultChan)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()

	require.True(t, st.watching)
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
}

// TestPreSignedTxPublishRetry asserts that a rejected publication is retried
// on every block for as long as it takes, past the deadline included: the
// deadline is an urgency target for the anchor child, not the point at
// which the output stops being claimable. Once the mempool accepts the
// parent, the anchor is registered and nothing is repeated afterwards.
func TestPreSignedTxPublishRetry(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)
	req.DeadlineHeight = fn.Some(int32(102))

	// A persistent fee rejection: the mempool minimum sits above the
	// parent's fixed fee rate. Submission at height 100 and the blocks
	// 101 to 105 all fail, three of them past the deadline.
	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(
		lnwallet.ErrMempoolFee,
	).Times(6)
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]

	for range 5 {
		h.nextBlock()
		require.False(t, st.published)
		require.False(t, st.anchorOffered)
		requireNoResult(t, resultChan)
		require.Contains(t, h.s.preSigned, tx.TxHash())
	}
	require.Greater(t, h.s.currentHeight, int32(102),
		"the test must run past the deadline")

	// The fee market relaxes and the parent is accepted: CPFP follows.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()

	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	require.Contains(t, h.s.inputs, anchor.OutPoint())

	// Further blocks must not re-publish or re-register (the mocks are
	// .Once(), so a second call would fail the test).
	h.nextBlock()
	h.nextBlock()
	requireNoResult(t, resultChan)
}

// TestPreSignedTxNoDeadlineKeepsRetrying asserts that without a deadline a
// failing publication is retried indefinitely as well.
func TestPreSignedTxNoDeadlineKeepsRetrying(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 0)
	req := newRequest(tx, nil)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(
		errors.New("rejected"),
	).Times(6)
	resultChan := h.submit(t, req)

	for range 5 {
		h.nextBlock()
	}
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())
}

// TestPreSignedTxAnchorRegistrationRetry asserts the registration
// acknowledgement boundary: when the anchor cannot be registered (spend
// notification registration fails), the lifecycle does NOT consider the
// anchor offered, and retries the registration on every block, past the
// deadline included, without repeating the publication.
func TestPreSignedTxAnchorRegistrationRetry(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()
	req := newRequest(tx, anchor)
	req.DeadlineHeight = fn.Some(int32(101))

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.notifier.On(
		"RegisterSpendNtfn", &op, mock.Anything, mock.Anything,
	).Return(nil, errors.New("notifier down")).Times(3)

	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]
	require.True(t, st.published, "publication must not be redone")
	require.False(t, st.anchorOffered,
		"a failed registration is not an acknowledgement")
	requireNoResult(t, resultChan)

	// Blocks 101 and 102 (past the deadline) retry and fail again.
	h.nextBlock()
	h.nextBlock()
	require.False(t, st.anchorOffered)
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())

	// Block 103: the registration succeeds.
	h.expectSpendRegistration(op)
	h.nextBlock()
	require.True(t, st.anchorOffered)
	require.Contains(t, h.s.inputs, op)
	require.Equal(t, Init, h.s.inputs[op].state)
}

// TestPreSignedTxWithoutAnchor asserts that a tx without an anchor concludes
// successfully as soon as it is published, with the tx itself as the result,
// and that its input watch is cancelled.
func TestPreSignedTxWithoutAnchor(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 0)
	req := newRequest(tx, nil)

	watch := h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	resultChan := h.submit(t, req)

	result := requireResult(t, resultChan)
	require.NoError(t, result.Err)
	require.Equal(t, tx.TxHash(), result.Tx.TxHash())
	require.NotContains(t, h.s.preSigned, tx.TxHash())
	require.EqualValues(t, 1, watch.cancelled.Load())
}

// TestPreSignedTxDuplicateRequest asserts that re-submitting an in-flight tx
// (resolver relaunch) attaches an additional listener instead of restarting
// the lifecycle, and that every listener receives the final result.
func TestPreSignedTxDuplicateRequest(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())

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

// TestPreSignedTxAnchorOutcomes asserts which anchor sweep results conclude
// the lifecycle: our child confirming, and a third-party spend of the
// anchor. Both imply the parent confirmed.
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
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newPreSignedTestHarness(t)
			tx, anchor := newPreSignedTx(t, 0)
			req := newRequest(tx, anchor)

			watch := h.expectInputWatch(req)
			h.wallet.On("PublishTransaction", tx, "test").
				Return(nil).Once()
			h.expectSpendRegistration(anchor.OutPoint())
			resultChan := h.submit(t, req)
			requireNoResult(t, resultChan)

			h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
				txid:   tx.TxHash(),
				result: tc.result,
			})

			result := requireResult(t, resultChan)
			require.Equal(t, tc.result, result)
			require.NotContains(t, h.s.preSigned, tx.TxHash())
			require.EqualValues(t, 1, watch.cancelled.Load())

			// No re-offer must follow a final result.
			h.nextBlock()
			requireNoResult(t, resultChan)
		})
	}
}

// TestPreSignedTxAnchorFailureRepublishes asserts that a terminal failure of
// the anchor sweep other than a third-party spend does not conclude the
// lifecycle: the output is still ours to claim, so the lifecycle goes back
// to publishing the parent and offers the anchor again on the next block.
func TestPreSignedTxAnchorFailureRepublishes(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]

	h.s.inputs[anchor.OutPoint()].state = Fatal
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Err: ErrNotEnoughBudget},
	})
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())
	require.False(t, st.published)
	require.False(t, st.anchorOffered)

	// The next block re-publishes (a no-op for a tx still in the
	// mempool) and registers the anchor afresh.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	requireNoResult(t, resultChan)
}

// TestPreSignedTxAnchorNotBatched asserts that the anchor is registered with
// input batching disabled. It spends an unconfirmed parent, so a missing
// parent must only ever fail the anchor's own sweep tx, never the sweeps of
// unrelated inputs that happen to share its deadline.
func TestPreSignedTxAnchorNotBatched(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)
	req.Budget = 1_000
	req.DeadlineHeight = fn.Some(int32(200))

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.submit(t, req)

	pi, ok := h.s.inputs[anchor.OutPoint()]
	require.True(t, ok)
	require.True(t, pi.params.NoInputBatch)
	require.Nil(t, pi.params.ExclusiveGroup)
	require.Equal(t, btcutil.Amount(1_000), pi.params.Budget)
	require.Equal(t, fn.Some(int32(200)), pi.params.DeadlineHeight,
		"the deadline drives the fee rate of the child")
}

// TestPreSignedTxParentEvicted asserts that a missing-input failure of the
// anchor sweep does not conclude the lifecycle. The parent left the mempool
// (evicted or replaced) and nobody else re-broadcasts it, so the lifecycle
// must go back to publishing the parent and then register the anchor again.
func TestPreSignedTxParentEvicted(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]
	require.True(t, st.anchorOffered)

	// The child fails because the parent is gone. The lifecycle is not
	// concluded and falls back to the publish step.
	h.failAnchorMissingParent(tx, anchor)
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())
	require.False(t, st.published)
	require.False(t, st.anchorOffered)

	// The next block re-broadcasts the parent. The first attempt is
	// rejected (the mempool minimum is still above its fee rate), which
	// is retried like any other failed publication.
	h.wallet.On("PublishTransaction", tx, "test").Return(
		lnwallet.ErrMempoolFee,
	).Once()
	h.nextBlock()
	require.False(t, st.published)
	require.NotContains(t, h.s.inputs, anchor.OutPoint())
	requireNoResult(t, resultChan)

	// Once the parent is accepted again, the anchor is registered anew.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	require.Contains(t, h.s.inputs, anchor.OutPoint())
	requireNoResult(t, resultChan)

	// A final result of the re-registered anchor concludes as usual.
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Err: ErrRemoteSpend},
	})
	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrRemoteSpend)
}

// TestPreSignedTxParentEvictedPastDeadline asserts that an eviction of the
// parent after the deadline is handled like one before it: the output is
// still claimable, so the parent is published again.
func TestPreSignedTxParentEvictedPastDeadline(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)
	req.DeadlineHeight = fn.Some(int32(101))

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]

	h.nextBlock() // 101
	h.nextBlock() // 102, past the deadline.
	h.failAnchorMissingParent(tx, anchor)
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock() // 103
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	requireNoResult(t, resultChan)
}

// TestPreSignedTxDoubleSpendError asserts that a publication rejected as a
// double spend is not taken as proof of a confirmed conflict: the backend
// maps a missing input to the same error, and a commitment that is gone
// after a re-org comes back. The publication is retried like any other
// failure, and succeeds once the backend sees the input again.
func TestPreSignedTxDoubleSpendError(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	// No mempool is configured, as on neutrino: there is nothing else
	// to consult, and the lifecycle must not conclude on its own.
	require.Nil(t, h.s.cfg.Mempool)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(
		lnwallet.ErrDoubleSpend,
	).Times(3)
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]

	h.nextBlock()
	h.nextBlock()
	requireNoResult(t, resultChan)
	require.Contains(t, h.s.preSigned, tx.TxHash())
	require.False(t, st.published)
	require.NotContains(t, h.s.inputs, anchor.OutPoint())

	// The input is back (the re-org settled) and the publication goes
	// through.
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.nextBlock()
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	requireNoResult(t, resultChan)
}

// TestPreSignedTxConflictingSpend asserts that a confirmed spend of the
// parent's input by a different transaction ends the lifecycle with
// ErrPreSignedTxConflict, whether the parent was still being published or
// its anchor was already being swept, and that the input watch is cancelled.
func TestPreSignedTxConflictingSpend(t *testing.T) {
	t.Parallel()

	t.Run("while publishing", func(t *testing.T) {
		t.Parallel()

		h := newPreSignedTestHarness(t)
		tx, anchor := newPreSignedTx(t, 0)
		req := newRequest(tx, anchor)

		watch := h.expectInputWatch(req)
		h.wallet.On("PublishTransaction", tx, "test").Return(
			lnwallet.ErrDoubleSpend,
		).Once()
		resultChan := h.submit(t, req)
		requireNoResult(t, resultChan)

		conflict := conflictingTx(tx)
		h.spendInput(tx, conflict)

		result := requireResult(t, resultChan)
		require.ErrorIs(t, result.Err, ErrPreSignedTxConflict)
		require.ErrorContains(t, result.Err, conflict.TxHash().String())
		require.NotContains(t, h.s.preSigned, tx.TxHash())
		require.NotContains(t, h.s.inputs, anchor.OutPoint())
		require.EqualValues(t, 1, watch.cancelled.Load())

		// Nothing is retried after the final result.
		h.nextBlock()
		requireNoResult(t, resultChan)
	})

	t.Run("while sweeping the anchor", func(t *testing.T) {
		t.Parallel()

		h := newPreSignedTestHarness(t)
		tx, anchor := newPreSignedTx(t, 0)
		req := newRequest(tx, anchor)

		watch := h.expectInputWatch(req)
		h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
		h.expectSpendRegistration(anchor.OutPoint())
		resultChan := h.submit(t, req)
		require.True(t, h.s.preSigned[tx.TxHash()].anchorOffered)

		h.spendInput(tx, conflictingTx(tx))

		result := requireResult(t, resultChan)
		require.ErrorIs(t, result.Err, ErrPreSignedTxConflict)
		require.NotContains(t, h.s.preSigned, tx.TxHash())
		require.EqualValues(t, 1, watch.cancelled.Load())

		// The anchor sweep reports the missing parent later; the
		// lifecycle is gone and the result is ignored.
		h.failAnchorMissingParent(tx, anchor)
		requireNoResult(t, resultChan)
		require.Empty(t, h.s.preSigned)
	})
}

// TestPreSignedTxConfirmedByNotifier asserts that the chain notifier
// reporting the parent itself as the spender of its input is what marks the
// parent confirmed: publication stops being attempted and the anchor is
// registered, even when every publication attempt was rejected (the tx was
// broadcast by someone else, or confirmed while we were offline). Without
// an anchor the lifecycle concludes with the tx.
func TestPreSignedTxConfirmedByNotifier(t *testing.T) {
	t.Parallel()

	t.Run("with anchor", func(t *testing.T) {
		t.Parallel()

		h := newPreSignedTestHarness(t)
		tx, anchor := newPreSignedTx(t, 0)
		req := newRequest(tx, anchor)

		h.expectInputWatch(req)
		h.wallet.On("PublishTransaction", tx, "test").Return(
			lnwallet.ErrDoubleSpend,
		).Twice()
		resultChan := h.submit(t, req)
		h.nextBlock()
		st := h.s.preSigned[tx.TxHash()]
		require.False(t, st.published)

		// The parent itself confirmed. The anchor is registered right
		// away and no further publication is attempted (the publish
		// mock is exhausted).
		h.expectSpendRegistration(anchor.OutPoint())
		h.spendInput(tx, tx)
		require.True(t, st.confirmed)
		require.True(t, st.anchorOffered)
		require.Contains(t, h.s.inputs, anchor.OutPoint())
		requireNoResult(t, resultChan)

		h.nextBlock()
		requireNoResult(t, resultChan)
	})

	t.Run("without anchor", func(t *testing.T) {
		t.Parallel()

		h := newPreSignedTestHarness(t)
		tx, _ := newPreSignedTx(t, 0)
		req := newRequest(tx, nil)

		watch := h.expectInputWatch(req)
		h.wallet.On("PublishTransaction", tx, "test").Return(
			errors.New("rejected"),
		).Once()
		resultChan := h.submit(t, req)
		requireNoResult(t, resultChan)

		h.spendInput(tx, tx)

		result := requireResult(t, resultChan)
		require.NoError(t, result.Err)
		require.Equal(t, tx.TxHash(), result.Tx.TxHash())
		require.NotContains(t, h.s.preSigned, tx.TxHash())
		require.EqualValues(t, 1, watch.cancelled.Load())
	})
}

// TestPreSignedTxRestartConfirmedParent runs a re-submission after a restart
// through the live collector, for a parent that already confirmed and whose
// anchor was since swept by someone else. The broadcast is a no-op, the
// anchor is registered, and the historical spend of the anchor concludes the
// lifecycle with ErrRemoteSpend.
func TestPreSignedTxRestartConfirmedParent(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()
	req := newRequest(tx, anchor)
	req.Budget = 10_000

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	anchorWatch := h.expectSpendRegistration(op)

	thirdParty := wire.NewMsgTx(2)
	thirdParty.AddTxIn(&wire.TxIn{PreviousOutPoint: op})
	thirdPartyHash := thirdParty.TxHash()
	h.store.On("IsOurTx", thirdPartyHash).Return(false).Once()
	h.store.On("ListSweeps").Return([]chainhash.Hash{}, nil).Once()

	beat := &chainio.MockBlockbeat{}
	beat.On("Height").Return(int32(100)).Maybe()
	beat.On("logger").Return(log).Maybe()
	require.NoError(t, h.s.Start(beat))
	t.Cleanup(func() {
		require.NoError(t, h.s.Stop())
	})

	resultChan, err := h.s.PublishPreSignedTx(req)
	require.NoError(t, err)

	anchorWatch.spend <- &chainntnfs.SpendDetail{
		SpentOutPoint: &op,
		SpendingTx:    thirdParty,
		SpenderTxHash: &thirdPartyHash,
	}

	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrRemoteSpend)
}

// TestPreSignedTxUnknownEvents asserts that a late anchor result or input
// spend for a lifecycle that already concluded is ignored.
func TestPreSignedTxUnknownEvents(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 0)

	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Err: ErrRemoteSpend},
	})
	h.spendInput(tx, conflictingTx(tx))
	require.Empty(t, h.s.preSigned)
}

// TestPreSignedTxShutdown asserts that pending lifecycles are released with
// ErrSweeperShuttingDown when the sweeper stops, and their watches are
// cancelled.
func TestPreSignedTxShutdown(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, _ := newPreSignedTx(t, 500)
	req := newRequest(tx, nil)

	watch := h.expectInputWatch(req)
	resultChan := h.submit(t, req)
	requireNoResult(t, resultChan)

	h.s.failPreSignedTxs(ErrSweeperShuttingDown)

	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrSweeperShuttingDown)
	require.Empty(t, h.s.preSigned)
	require.EqualValues(t, 1, watch.cancelled.Load())
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
	req := newRequest(tx, anchor)
	req.Budget = 10_000
	req.DeadlineHeight = fn.Some(int32(200))

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	anchorWatch := h.expectSpendRegistration(op)

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

	resultChan, err := h.s.PublishPreSignedTx(req)
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
	anchorWatch.spend <- &chainntnfs.SpendDetail{
		SpentOutPoint: &op,
		SpendingTx:    child,
		SpenderTxHash: &childHash,
	}

	result := requireResult(t, resultChan)
	require.NoError(t, result.Err)
	require.Equal(t, childHash, result.Tx.TxHash())
}

// TestPreSignedTxEndToEndConflict runs a confirmed conflict through the live
// collector: the spend delivered by the input watch ends the lifecycle.
func TestPreSignedTxEndToEndConflict(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	inputWatch := h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())

	beat := &chainio.MockBlockbeat{}
	beat.On("Height").Return(int32(100)).Maybe()
	beat.On("logger").Return(log).Maybe()
	require.NoError(t, h.s.Start(beat))
	t.Cleanup(func() {
		require.NoError(t, h.s.Stop())
	})

	resultChan, err := h.s.PublishPreSignedTx(req)
	require.NoError(t, err)

	conflict := conflictingTx(tx)
	conflictHash := conflict.TxHash()
	htlcOp := tx.TxIn[0].PreviousOutPoint
	inputWatch.spend <- &chainntnfs.SpendDetail{
		SpentOutPoint:  &htlcOp,
		SpendingTx:     conflict,
		SpenderTxHash:  &conflictHash,
		SpendingHeight: 100,
	}

	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrPreSignedTxConflict)
}
