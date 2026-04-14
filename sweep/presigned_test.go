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
	reorg     chan struct{}
	done      chan struct{}
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
		reorg: make(chan struct{}, 1),
		done:  make(chan struct{}, 1),
	}
	h.notifier.On(
		"RegisterSpendNtfn", &op, req.SpentOutputs[0].PkScript,
		req.HeightHint,
	).Return(&chainntnfs.SpendEvent{
		Spend: w.spend,
		Reorg: w.reorg,
		Done:  w.done,
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

// reorgInput hands the collector the re-org of the spend last reported for
// the input of the given tx, the way the input watch delivers it.
func (h *preSignedTestHarness) reorgInput(tx *wire.MsgTx) {
	h.s.handlePreSignedSpend(&preSignedSpend{
		txid:    tx.TxHash(),
		reorged: true,
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

	// anchorAt builds an anchor input claiming the given output index of
	// tx, described by the given output.
	anchorAt := func(index uint32, txOut *wire.TxOut) input.Input {
		op := wire.OutPoint{Hash: tx.TxHash(), Index: index}
		anchor := input.MakeBaseInput(
			&op, input.TaprootAnchorSweepSpend,
			&input.SignDescriptor{Output: txOut}, 100, nil,
		)

		return &anchor
	}
	anchorOut := tx.TxOut[1]

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
			Tx: tx,
			SpentOutputs: append(
				spentOutputs(tx), spentOutputs(otherTx)...,
			),
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
		name: "anchor index past the last output",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor:       anchorAt(2, anchorOut),
		},
	}, {
		name: "anchor without output",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor:       anchorAt(1, nil),
		},
	}, {
		name: "anchor with the script of another output",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor: anchorAt(1, &wire.TxOut{
				Value:    anchorOut.Value,
				PkScript: tx.TxOut[0].PkScript,
			}),
		},
	}, {
		name: "anchor with another value",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor: anchorAt(1, &wire.TxOut{
				Value:    anchorOut.Value + 1,
				PkScript: anchorOut.PkScript,
			}),
		},
	}, {
		name: "anchor at another index than 1",
		req: PreSignedTxRequest{
			Tx:           tx,
			SpentOutputs: spentOutputs(tx),
			Anchor:       anchorAt(0, tx.TxOut[0]),
		},
		ok: true,
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
			if tc.ok {
				require.NoError(
					t, validatePreSignedTxRequest(tc.req),
				)

				return
			}

			// The public API rejects the request before it
			// reaches the collector. The collector is not
			// running, so a request that is let through would
			// block: run the call on the side to fail instead.
			errChan := make(chan error, 1)
			go func() {
				_, err := h.s.PublishPreSignedTx(tc.req)
				errChan <- err
			}()

			select {
			case err := <-errChan:
				require.ErrorIs(
					t, err, ErrInvalidPreSignedTxRequest,
				)

			case <-time.After(5 * time.Second):
				t.Fatal("request was not rejected")
			}
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

// TestPreSignedTxConfirmedWhileAnchorFatal asserts that the anchor is not
// offered to the sweeper while its previous, failed input is still tracked.
// The child failing with a missing parent leaves that input behind in a
// final state until the next block prunes it. If the parent's confirmation
// is reported in that window, offering the anchor would attach the lifecycle
// to the dead entry, which is then deleted without a result, leaving nothing
// to sweep the anchor and nothing to end the lifecycle.
func TestPreSignedTxConfirmedWhileAnchorFatal(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(op)
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]
	require.True(t, st.anchorOffered)

	// The child fails, the anchor input stays behind as fatal.
	h.failAnchorMissingParent(tx, anchor)
	deadInput := h.s.inputs[op]
	require.Equal(t, Fatal, deadInput.state)
	numListeners := len(deadInput.listeners)

	// The parent turns out to have confirmed, before the next block. The
	// anchor must not be attached to the dead input.
	h.spendInput(tx, tx)
	require.True(t, st.confirmed)
	require.False(t, st.anchorOffered)
	require.Len(t, deadInput.listeners, numListeners)
	requireNoResult(t, resultChan)

	// The next block prunes the dead input and registers the anchor
	// afresh, with a new spend watch, without publishing the confirmed
	// parent again.
	h.expectSpendRegistration(op)
	h.nextBlock()
	require.True(t, st.anchorOffered)
	liveInput, ok := h.s.inputs[op]
	require.True(t, ok)
	require.NotSame(t, deadInput, liveInput)
	require.Equal(t, Init, liveInput.state)
	requireNoResult(t, resultChan)
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 1)

	// The live input still ends the lifecycle.
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Tx: tx},
	})
	require.NoError(t, requireResult(t, resultChan).Err)
}

// TestPreSignedTxAnchorFinalStates asserts that the anchor is held back for
// every final state its previous input can be left in, and offered for a
// state that is not final.
func TestPreSignedTxAnchorFinalStates(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		state    SweepState
		heldBack bool
	}{
		{state: Fatal, heldBack: true},
		{state: Swept, heldBack: true},
		{state: Excluded, heldBack: true},
		{state: PublishFailed},
	}

	for _, tc := range testCases {
		t.Run(tc.state.String(), func(t *testing.T) {
			t.Parallel()

			h := newPreSignedTestHarness(t)
			tx, anchor := newPreSignedTx(t, 0)
			req := newRequest(tx, anchor)

			// Another party already has the anchor tracked.
			h.s.inputs[anchor.OutPoint()] = &SweeperInput{
				Input: anchor,
				state: tc.state,
			}

			h.expectInputWatch(req)
			h.wallet.On("PublishTransaction", tx, "test").Return(
				nil,
			).Once()
			h.submit(t, req)

			st := h.s.preSigned[tx.TxHash()]
			require.True(t, st.published)
			require.Equal(t, !tc.heldBack, st.anchorOffered)
		})
	}
}

// TestPreSignedTxParentReorged asserts that a confirmation of the pre-signed
// tx that is re-orged out makes the tx eligible for publication again: it is
// re-broadcast right away and, if rejected, on every block, and once its
// child reports it missing the anchor is registered anew.
func TestPreSignedTxParentReorged(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	op := anchor.OutPoint()
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(op)
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]

	// The parent confirms. Nothing is published while it is.
	h.spendInput(tx, tx)
	require.True(t, st.confirmed)
	h.nextBlock()
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 1)

	// The confirmation is re-orged out. The parent is published again
	// right away, although its anchor is still being swept. The mempool
	// rejects it, so the next block tries again.
	h.wallet.On("PublishTransaction", tx, "test").Return(
		lnwallet.ErrMempoolFee,
	).Once()
	h.reorgInput(tx)
	require.False(t, st.confirmed)
	require.False(t, st.published)
	require.True(t, st.anchorOffered)
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 2)
	requireNoResult(t, resultChan)

	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.nextBlock()
	require.True(t, st.published)
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 3)

	// Once published, later blocks leave it alone, and the anchor that
	// is still being swept is not registered a second time.
	h.nextBlock()
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 3)
	h.notifier.AssertNumberOfCalls(t, "RegisterSpendNtfn", 2)

	// The child reports the parent as missing after all: the parent is
	// published and the anchor registered again on the next block.
	h.failAnchorMissingParent(tx, anchor)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(op)
	h.nextBlock()
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 4)

	// The parent confirms again and the lifecycle ends as usual.
	h.spendInput(tx, tx)
	require.True(t, st.confirmed)
	h.s.handlePreSignedAnchorResult(&preSignedAnchorResult{
		txid:   tx.TxHash(),
		result: Result{Err: ErrRemoteSpend},
	})
	require.ErrorIs(t, requireResult(t, resultChan).Err, ErrRemoteSpend)
}

// TestPreSignedTxReorgThenConflict asserts that after its confirmation was
// re-orged out, the pre-signed tx can still lose to a conflicting spend,
// which ends the lifecycle.
func TestPreSignedTxReorgThenConflict(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	inputWatch := h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	resultChan := h.submit(t, req)

	h.spendInput(tx, tx)
	h.wallet.On("PublishTransaction", tx, "test").Return(
		lnwallet.ErrDoubleSpend,
	).Once()
	h.reorgInput(tx)
	requireNoResult(t, resultChan)

	h.spendInput(tx, conflictingTx(tx))
	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrPreSignedTxConflict)
	require.NotContains(t, h.s.preSigned, tx.TxHash())
	require.EqualValues(t, 1, inputWatch.cancelled.Load())
}

// TestPreSignedTxReorgNotConfirmed asserts that a re-org reported for a tx
// that was never seen confirmed publishes it again. The notifier drops a
// spend nobody read yet when it dispatches the re-org, so the re-org can be
// the first event of an input. The anchor that is already being swept is not
// registered a second time.
func TestPreSignedTxReorgNotConfirmed(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)

	h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Twice()
	h.expectSpendRegistration(anchor.OutPoint())
	resultChan := h.submit(t, req)
	st := h.s.preSigned[tx.TxHash()]
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 1)

	h.reorgInput(tx)
	require.False(t, st.confirmed)
	require.True(t, st.published)
	require.True(t, st.anchorOffered)
	h.wallet.AssertNumberOfCalls(t, "PublishTransaction", 2)
	h.notifier.AssertNumberOfCalls(t, "RegisterSpendNtfn", 2)
	requireNoResult(t, resultChan)
}

// TestPreSignedTxInputWatchEndsOnDone asserts that the watch on an input
// ends once the notifier is done with the registration, and that a spend
// that is still unread at that point is forwarded first.
func TestPreSignedTxInputWatchEndsOnDone(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name         string
		pendingSpend bool
	}{{
		name: "no pending spend",
	}, {
		name:         "pending spend",
		pendingSpend: true,
	}}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newPreSignedTestHarness(t)
			tx, anchor := newPreSignedTx(t, 0)
			req := newRequest(tx, anchor)
			txid := tx.TxHash()

			// Both events are in place before the watch starts,
			// so they are ready in the same iteration. Whichever
			// the watch picks first, the spend is forwarded once.
			inputWatch := h.expectInputWatch(req)
			spend := &chainntnfs.SpendDetail{SpenderTxHash: &txid}
			inputWatch.done <- struct{}{}
			if tc.pendingSpend {
				inputWatch.spend <- spend
			}

			h.wallet.On(
				"PublishTransaction", tx, "test",
			).Return(nil).Once()
			h.expectSpendRegistration(anchor.OutPoint())
			h.submit(t, req)

			if tc.pendingSpend {
				select {
				case event := <-h.s.preSignedSpends:
					require.Same(t, spend, event.spend)
				case <-time.After(5 * time.Second):
					t.Fatal("pending spend not forwarded")
				}
			}

			// The watch is gone: nothing sent afterwards is
			// forwarded.
			require.Eventually(t, func() bool {
				return len(inputWatch.done) == 0
			}, 5*time.Second, 10*time.Millisecond)
			inputWatch.reorg <- struct{}{}
			select {
			case event := <-h.s.preSignedSpends:
				t.Fatalf("unexpected event: %v", event)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestPreSignedTxInputWatchOutlivesSpend asserts that the watch on an input
// keeps forwarding events after the first spend: the re-org of that spend
// and the spend that follows it.
func TestPreSignedTxInputWatchOutlivesSpend(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	req := newRequest(tx, anchor)
	txid := tx.TxHash()

	inputWatch := h.expectInputWatch(req)
	h.wallet.On("PublishTransaction", tx, "test").Return(nil).Once()
	h.expectSpendRegistration(anchor.OutPoint())
	h.submit(t, req)

	nextEvent := func() *preSignedSpend {
		t.Helper()

		select {
		case event := <-h.s.preSignedSpends:
			return event
		case <-time.After(5 * time.Second):
			t.Fatal("no event forwarded")
		}

		return nil
	}

	spend := &chainntnfs.SpendDetail{SpenderTxHash: &txid}
	inputWatch.spend <- spend
	event := nextEvent()
	require.Equal(t, txid, event.txid)
	require.Same(t, spend, event.spend)
	require.False(t, event.reorged)

	inputWatch.reorg <- struct{}{}
	event = nextEvent()
	require.Equal(t, txid, event.txid)
	require.Nil(t, event.spend)
	require.True(t, event.reorged)

	conflictHash := conflictingTx(tx).TxHash()
	respend := &chainntnfs.SpendDetail{SpenderTxHash: &conflictHash}
	inputWatch.spend <- respend
	event = nextEvent()
	require.Same(t, respend, event.spend)
	require.False(t, event.reorged)

	// Cancelling the registration closes its channels, which ends the
	// watch without forwarding anything else.
	close(inputWatch.spend)
	close(inputWatch.reorg)
	select {
	case event := <-h.s.preSignedSpends:
		t.Fatalf("unexpected event: %v", event)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestPreSignedTxEndToEndReorg runs a confirmation, its re-org and the
// second confirmation through the live collector.
func TestPreSignedTxEndToEndReorg(t *testing.T) {
	t.Parallel()

	h := newPreSignedTestHarness(t)
	tx, anchor := newPreSignedTx(t, 0)
	txid := tx.TxHash()
	op := anchor.OutPoint()
	inOp := tx.TxIn[0].PreviousOutPoint
	req := newRequest(tx, anchor)

	inputWatch := h.expectInputWatch(req)
	published := make(chan struct{}, 2)
	h.wallet.On("PublishTransaction", tx, "test").Run(func(mock.Arguments) {
		published <- struct{}{}
	}).Return(nil).Twice()
	h.expectSpendRegistration(op)

	beat := &chainio.MockBlockbeat{}
	beat.On("Height").Return(int32(100)).Maybe()
	beat.On("logger").Return(log).Maybe()
	require.NoError(t, h.s.Start(beat))
	t.Cleanup(func() {
		require.NoError(t, h.s.Stop())
	})

	waitPublished := func() {
		t.Helper()

		select {
		case <-published:
		case <-time.After(5 * time.Second):
			t.Fatal("tx not published")
		}
	}

	resultChan, err := h.s.PublishPreSignedTx(req)
	require.NoError(t, err)
	waitPublished()

	// Confirm the tx, then re-org the confirmation out: the collector
	// publishes the tx a second time.
	inputWatch.spend <- &chainntnfs.SpendDetail{
		SpentOutPoint: &inOp,
		SpendingTx:    tx,
		SpenderTxHash: &txid,
	}

	// The notifier reports the two events on separate channels. Only
	// report the re-org once the watch has taken the spend, so that the
	// collector sees them in the order they happened.
	require.Eventually(t, func() bool {
		return len(inputWatch.spend) == 0
	}, 5*time.Second, time.Millisecond)
	inputWatch.reorg <- struct{}{}
	waitPublished()
	requireNoResult(t, resultChan)

	// A conflicting spend confirms instead and ends the lifecycle.
	conflict := conflictingTx(tx)
	conflictHash := conflict.TxHash()
	inputWatch.spend <- &chainntnfs.SpendDetail{
		SpentOutPoint: &inOp,
		SpendingTx:    conflict,
		SpenderTxHash: &conflictHash,
	}
	result := requireResult(t, resultChan)
	require.ErrorIs(t, result.Err, ErrPreSignedTxConflict)
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
	h.reorgInput(tx)
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
