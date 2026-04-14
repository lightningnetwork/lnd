package sweep

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
)

var (
	// ErrPreSignedTxDeadline is returned when a pre-signed transaction's
	// publication or anchor registration keeps failing and the deadline
	// height has passed, at which point the sweeper stops retrying.
	ErrPreSignedTxDeadline = errors.New("pre-signed tx deadline passed " +
		"before publication succeeded")
)

// PreSignedTxRequest asks the sweeper to take ownership of the lifecycle of a
// fully signed, immutable transaction: publish it verbatim once its locktime
// allows, keep re-broadcasting it on every block until the broadcast
// succeeds, and once it is in the mempool, CPFP it through the given anchor
// input. Such transactions cannot be rebuilt, aggregated or RBF'd by the
// sweeper (the counterparty's signature commits to the whole tx), so the
// anchor child is their only fee-bumping path.
type PreSignedTxRequest struct {
	// Tx is the fully signed transaction to publish verbatim.
	Tx *wire.MsgTx

	// Label is the wallet label attached to the broadcast.
	Label string

	// Anchor is the CPFP anchor output of Tx, as a sweepable input. It is
	// optional: without it the sweeper only publishes Tx.
	Anchor input.Input

	// Budget bounds the fees the sweeper may spend on the anchor child.
	Budget btcutil.Amount

	// DeadlineHeight is the height Tx should be confirmed by. It is passed
	// to the anchor sweep (where it drives fee bumping) and bounds how
	// long the sweeper keeps retrying a failing publication or anchor
	// registration.
	DeadlineHeight fn.Option[int32]
}

// preSignedTxMessage carries a PreSignedTxRequest into the collector loop.
type preSignedTxMessage struct {
	req        PreSignedTxRequest
	resultChan chan Result
}

// preSignedTxState is the collector-owned state of one pre-signed tx.
type preSignedTxState struct {
	req PreSignedTxRequest

	// listeners receive the single final Result of this lifecycle.
	listeners []chan Result

	// published is set once PublishTx accepted the transaction.
	published bool

	// anchorOffered is set once the anchor input is registered with the
	// sweeper's regular input machinery.
	anchorOffered bool
}

// preSignedAnchorResult carries the final result of an anchor sweep back
// into the collector loop.
type preSignedAnchorResult struct {
	txid   chainhash.Hash
	result Result
}

// PublishPreSignedTx hands a pre-signed transaction to the sweeper, which
// owns its lifecycle from here on: publication (gated on the tx locktime and
// retried on every block), anchor CPFP registration, and the final outcome.
//
// The returned channel receives exactly one Result: on success its Tx is the
// transaction that spent the anchor (or the pre-signed tx itself when there
// is no anchor to sweep). Error outcomes are final by the sweeper's own
// classification: the regular sweep machinery already retries fee bumps and
// publication failures internally and only reports terminal states (swept,
// remotely spent, fatal, budget exhausted). The only step the lifecycle
// retries on its own is the parent publication and the anchor registration,
// and those retries stop once the deadline height has passed.
//
// Nothing is persisted: like regular sweep inputs, the caller re-submits the
// request after a restart, at which point an already-published tx is a
// no-op broadcast and the anchor is simply re-registered.
func (s *UtxoSweeper) PublishPreSignedTx(
	req PreSignedTxRequest) (<-chan Result, error) {

	if req.Tx == nil {
		return nil, errors.New("nil pre-signed tx")
	}
	if req.Anchor != nil {
		if req.Anchor.OutPoint() == input.EmptyOutPoint ||
			req.Anchor.SignDesc() == nil {

			return nil, errors.New("invalid anchor input")
		}
		if req.Anchor.OutPoint().Hash != req.Tx.TxHash() {
			return nil, fmt.Errorf("anchor %v does not belong to "+
				"tx %v", req.Anchor.OutPoint(), req.Tx.TxHash())
		}
	}

	msg := &preSignedTxMessage{
		req:        req,
		resultChan: make(chan Result, 1),
	}

	select {
	case s.preSignedReqs <- msg:
	case <-s.quit:
		return nil, ErrSweeperShuttingDown
	}

	return msg.resultChan, nil
}

// handlePreSignedTxReq registers a new pre-signed tx with the collector and
// immediately attempts to make progress on it (publishing it if its locktime
// already allows), so a request made between blocks doesn't idle until the
// next one.
func (s *UtxoSweeper) handlePreSignedTxReq(msg *preSignedTxMessage) {
	txid := msg.req.Tx.TxHash()

	// A duplicate request (resolver relaunch, restart) attaches as an
	// additional listener to the in-flight lifecycle.
	if st, ok := s.preSigned[txid]; ok {
		log.Infof("Pre-signed tx %v already tracked, adding listener",
			txid)
		st.listeners = append(st.listeners, msg.resultChan)

		return
	}

	st := &preSignedTxState{
		req:       msg.req,
		listeners: []chan Result{msg.resultChan},
	}
	s.preSigned[txid] = st

	log.Infof("Registered pre-signed tx %v (locktime=%d, anchor=%v, "+
		"budget=%v, deadline=%v) at height %d", txid,
		msg.req.Tx.LockTime, msg.req.Anchor != nil, msg.req.Budget,
		msg.req.DeadlineHeight, s.currentHeight)

	s.progressPreSignedTx(txid, st)
}

// processPreSignedTxs is called on every new block and drives each tracked
// pre-signed tx one step further.
func (s *UtxoSweeper) processPreSignedTxs() {
	for txid, st := range s.preSigned {
		s.progressPreSignedTx(txid, st)
	}
}

// progressPreSignedTx performs whatever step the given pre-signed tx is
// waiting for at the current height: publication, then anchor registration.
// Failures of either step are retried on the next block until the deadline
// has passed, at which point the lifecycle ends with ErrPreSignedTxDeadline.
func (s *UtxoSweeper) progressPreSignedTx(txid chainhash.Hash,
	st *preSignedTxState) {

	if st.anchorOffered {
		return
	}

	// Step 1: publication, gated on the locktime. A locktime'd tx is only
	// final once the chain has reached that height.
	if !st.published {
		if uint32(s.currentHeight) < st.req.Tx.LockTime {
			log.Debugf("Pre-signed tx %v waiting for locktime %d "+
				"(height=%d)", txid, st.req.Tx.LockTime,
				s.currentHeight)

			return
		}

		err := s.cfg.Wallet.PublishTransaction(st.req.Tx, st.req.Label)
		if err != nil {
			s.retryOrExpirePreSignedTx(txid, st, fmt.Errorf(
				"publish pre-signed tx: %w", err))

			return
		}

		st.published = true
		log.Infof("Published pre-signed tx %v at height %d", txid,
			s.currentHeight)
	}

	// Step 2: without an anchor there is nothing left to supervise.
	if st.req.Anchor == nil {
		s.finishPreSignedTx(txid, st, Result{Tx: st.req.Tx})

		return
	}

	// Register the anchor with the regular sweep machinery. This runs
	// inline in the collector, so a nil error is a real acknowledgement
	// that the spend notification is registered and the input is being
	// swept; from here on the sweeper's own retry and fee-bumping logic
	// owns the child, and the result it eventually reports is final.
	anchorResult := make(chan Result, 1)
	err := s.handleNewInput(&sweepInputMessage{
		input: st.req.Anchor,
		params: Params{
			Budget:         st.req.Budget,
			DeadlineHeight: st.req.DeadlineHeight,
		},
		resultChan: anchorResult,
	})
	if err != nil {
		s.retryOrExpirePreSignedTx(txid, st, fmt.Errorf(
			"register anchor of pre-signed tx: %w", err))

		return
	}

	st.anchorOffered = true
	log.Infof("Registered CPFP anchor %v of pre-signed tx %v (budget=%v, "+
		"deadline=%v)", st.req.Anchor.OutPoint(), txid, st.req.Budget,
		st.req.DeadlineHeight)

	// Forward the anchor's final result back into the collector so the
	// lifecycle can conclude without blocking the main loop.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		select {
		case result, ok := <-anchorResult:
			if !ok {
				return
			}

			select {
			case s.preSignedAnchorResults <- &preSignedAnchorResult{
				txid:   txid,
				result: result,
			}:
			case <-s.quit:
			}

		case <-s.quit:
		}
	}()
}

// retryOrExpirePreSignedTx logs a failed publication or registration step and
// leaves the tx to be retried on the next block, unless the deadline has
// passed, in which case the lifecycle is concluded with
// ErrPreSignedTxDeadline.
func (s *UtxoSweeper) retryOrExpirePreSignedTx(txid chainhash.Hash,
	st *preSignedTxState, stepErr error) {

	deadlinePassed := false
	st.req.DeadlineHeight.WhenSome(func(d int32) {
		deadlinePassed = s.currentHeight > d
	})

	if !deadlinePassed {
		log.Warnf("Pre-signed tx %v: %v, retrying on next block "+
			"(height=%d, deadline=%v)", txid, stepErr,
			s.currentHeight, st.req.DeadlineHeight)

		return
	}

	log.Errorf("Pre-signed tx %v: %v, deadline %v passed at height %d, "+
		"giving up", txid, stepErr, st.req.DeadlineHeight,
		s.currentHeight)

	s.finishPreSignedTx(txid, st, Result{
		Err: fmt.Errorf("%w: %w", ErrPreSignedTxDeadline, stepErr),
	})
}

// handlePreSignedAnchorResult concludes a pre-signed tx lifecycle with the
// final result of its anchor sweep.
func (s *UtxoSweeper) handlePreSignedAnchorResult(r *preSignedAnchorResult) {
	st, ok := s.preSigned[r.txid]
	if !ok {
		log.Debugf("Anchor result for unknown pre-signed tx %v", r.txid)

		return
	}

	switch {
	case errors.Is(r.result.Err, ErrRemoteSpend):
		// Someone else spent the anchor. Once the parent has been
		// confirmed for 16 blocks the output is anyone-can-spend, so
		// this is the expected outcome when our own child never
		// confirmed. Either way CPFP is moot now.
		log.Debugf("Anchor of pre-signed tx %v spent by a third party",
			r.txid)

	case r.result.Err != nil:
		log.Errorf("Anchor sweep of pre-signed tx %v failed "+
			"terminally: %v", r.txid, r.result.Err)

	default:
		if r.result.Tx != nil {
			log.Infof("Anchor of pre-signed tx %v swept by tx=%v",
				r.txid, r.result.Tx.TxHash())
		}
	}

	s.finishPreSignedTx(r.txid, st, r.result)
}

// finishPreSignedTx delivers the final result to every listener and drops the
// tx from the tracked set.
func (s *UtxoSweeper) finishPreSignedTx(txid chainhash.Hash,
	st *preSignedTxState, result Result) {

	// Channels are buffered and only ever written once, so this never
	// blocks.
	for _, listener := range st.listeners {
		listener <- result
	}

	delete(s.preSigned, txid)
}

// failPreSignedTxs concludes every tracked pre-signed tx with the given error.
// Used on shutdown so callers blocked on a result are released.
func (s *UtxoSweeper) failPreSignedTxs(err error) {
	for txid, st := range s.preSigned {
		s.finishPreSignedTx(txid, st, Result{Err: err})
	}
}
