package sweep

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
)

var (
	// ErrInvalidPreSignedTxRequest is returned by PublishPreSignedTx when
	// the request is malformed. The wrapped error names the field.
	ErrInvalidPreSignedTxRequest = errors.New("invalid pre-signed tx " +
		"request")

	// ErrPreSignedTxConflict is returned when a pre-signed transaction
	// can never confirm because one of its inputs was spent by a
	// different, confirmed transaction.
	ErrPreSignedTxConflict = errors.New("pre-signed tx input spent by " +
		"a conflicting transaction")
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

	// SpentOutputs are the outputs Tx spends, one per input of Tx and in
	// the same order. The sweeper watches them for a confirmed spend,
	// which is the only evidence it accepts that Tx either confirmed or
	// can never confirm.
	SpentOutputs []*wire.TxOut

	// HeightHint is the height from which the inputs of Tx are watched,
	// so a spend that confirmed while we were offline is still found.
	HeightHint uint32

	// Label is the wallet label attached to the broadcast.
	Label string

	// Anchor is the CPFP anchor output of Tx, as a sweepable input. It is
	// optional: without it the sweeper only publishes Tx.
	Anchor input.Input

	// Budget bounds the fees the sweeper may spend on the anchor child.
	Budget btcutil.Amount

	// DeadlineHeight is the height Tx should be confirmed by. It is passed
	// to the anchor sweep, where it drives the fee rate of the child. It
	// does not bound the lifecycle: the output Tx claims stays claimable
	// past that height, so publication and anchor registration keep being
	// retried until an input of Tx is spent on chain.
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

	// watching is set once a spend notification is registered for every
	// input of the tx.
	watching bool

	// cancelWatch cancels the registered spend notifications.
	cancelWatch []func()

	// confirmed is set once the chain notifier reported the tx itself as
	// the spender of one of its inputs. From then on there is nothing
	// left to publish.
	confirmed bool

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

// preSignedSpend carries a confirmed spend of one of the inputs of a
// pre-signed tx into the collector loop.
type preSignedSpend struct {
	txid  chainhash.Hash
	spend *chainntnfs.SpendDetail
}

// PublishPreSignedTx hands a pre-signed transaction to the sweeper, which
// owns its lifecycle from here on: publication (gated on the tx locktime and
// retried on every block), anchor CPFP registration, and the final outcome.
//
// The returned channel receives exactly one Result. The lifecycle ends in
// one of these ways only:
//   - our anchor child confirmed: Result.Tx is the child (or the pre-signed
//     tx itself when there is no anchor to sweep and the tx was published).
//   - the anchor was spent by someone else: ErrRemoteSpend.
//   - an input of the pre-signed tx was spent by a different, confirmed
//     transaction: ErrPreSignedTxConflict.
//   - the sweeper shut down: ErrSweeperShuttingDown.
//
// Everything else is retried on every block: a rejected publication, a
// failed anchor registration, and an anchor sweep that failed because its
// parent left the mempool (evicted or replaced), which nobody else would
// re-broadcast. The deadline only drives the fee rate of the anchor child;
// it never ends the lifecycle, since the output the tx claims stays
// claimable past it. Whether the tx confirmed or can no longer confirm is
// decided by the chain notifier alone: a publication error that reports an
// input as spent is retried like any other, until the spend confirms.
//
// Nothing is persisted: like regular sweep inputs, the caller re-submits the
// request after a restart, at which point an already-published tx is a
// no-op broadcast and the anchor is simply re-registered.
func (s *UtxoSweeper) PublishPreSignedTx(
	req PreSignedTxRequest) (<-chan Result, error) {

	if err := validatePreSignedTxRequest(req); err != nil {
		return nil, err
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

// validatePreSignedTxRequest checks that a request is well formed: a tx with
// one spent output per input, and an anchor, when given, that is a well
// formed input actually belonging to the tx.
func validatePreSignedTxRequest(req PreSignedTxRequest) error {
	if req.Tx == nil {
		return fmt.Errorf("%w: nil tx", ErrInvalidPreSignedTxRequest)
	}

	if len(req.SpentOutputs) != len(req.Tx.TxIn) {
		return fmt.Errorf("%w: %d spent outputs for %d inputs",
			ErrInvalidPreSignedTxRequest, len(req.SpentOutputs),
			len(req.Tx.TxIn))
	}
	for i, txOut := range req.SpentOutputs {
		if txOut == nil || len(txOut.PkScript) == 0 {
			return fmt.Errorf("%w: spent output %d has no script",
				ErrInvalidPreSignedTxRequest, i)
		}
	}

	if req.Anchor == nil {
		return nil
	}

	if req.Anchor.OutPoint() == input.EmptyOutPoint ||
		req.Anchor.SignDesc() == nil {

		return fmt.Errorf("%w: anchor is not a well formed input",
			ErrInvalidPreSignedTxRequest)
	}
	if req.Anchor.OutPoint().Hash != req.Tx.TxHash() {
		return fmt.Errorf("%w: anchor %v does not belong to tx %v",
			ErrInvalidPreSignedTxRequest, req.Anchor.OutPoint(),
			req.Tx.TxHash())
	}

	return nil
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
// waiting for at the current height: watching its inputs, then publication,
// then anchor registration. A failed step is retried on the next block.
func (s *UtxoSweeper) progressPreSignedTx(txid chainhash.Hash,
	st *preSignedTxState) {

	if st.anchorOffered {
		return
	}

	// Step 0: watch the inputs of the tx. A confirmed spend of any of
	// them is what tells us the tx confirmed, or that it never will.
	// Without the watch in place we could publish and then miss the
	// spend, so this comes first.
	if !st.watching {
		if err := s.watchPreSignedTxInputs(txid, st); err != nil {
			s.retryPreSignedTx(txid, st, fmt.Errorf(
				"watch inputs of pre-signed tx: %w", err))

			return
		}
	}

	// Step 1: publication, gated on the locktime. A locktime'd tx is only
	// final once the chain has reached that height. A tx the chain
	// notifier already reported as confirmed has nothing to publish.
	if !st.published && !st.confirmed {
		if uint32(s.currentHeight) < st.req.Tx.LockTime {
			log.Debugf("Pre-signed tx %v waiting for locktime %d "+
				"(height=%d)", txid, st.req.Tx.LockTime,
				s.currentHeight)

			return
		}

		err := s.cfg.Wallet.PublishTransaction(st.req.Tx, st.req.Label)
		switch {
		// The backend reports an input as already spent. That is not
		// proof of anything: the backend maps a missing input to the
		// same error, a conflicting spend in the mempool can still be
		// evicted, and a commitment that is temporarily gone after a
		// re-org comes back. Only a confirmed spend reported by the
		// chain notifier ends the lifecycle, so keep retrying.
		case errors.Is(err, lnwallet.ErrDoubleSpend):
			s.retryPreSignedTx(txid, st, fmt.Errorf("publish "+
				"pre-signed tx, waiting for the chain "+
				"notifier to decide whether the reported "+
				"spend confirms: %w", err))

			return

		case err != nil:
			s.retryPreSignedTx(txid, st, fmt.Errorf(
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
	// owns the child.
	//
	// The anchor spends an unconfirmed parent, so it must not share a
	// sweep tx with any other input: if the parent is evicted or
	// replaced, the child fails with missing inputs and that failure
	// would otherwise take every batched input down with it.
	anchorResult := make(chan Result, 1)
	err := s.handleNewInput(&sweepInputMessage{
		input: st.req.Anchor,
		params: Params{
			Budget:         st.req.Budget,
			DeadlineHeight: st.req.DeadlineHeight,
			NoInputBatch:   true,
		},
		resultChan: anchorResult,
	})
	if err != nil {
		s.retryPreSignedTx(txid, st, fmt.Errorf(
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

// watchPreSignedTxInputs registers a spend notification for every input of
// the pre-signed tx and forwards the first spend of each into the collector.
// Either all inputs are watched afterwards or none is.
func (s *UtxoSweeper) watchPreSignedTxInputs(txid chainhash.Hash,
	st *preSignedTxState) error {

	cancels := make([]func(), 0, len(st.req.Tx.TxIn))
	cancelAll := func() {
		for _, cancel := range cancels {
			cancel()
		}
	}

	for i, txIn := range st.req.Tx.TxIn {
		op := txIn.PreviousOutPoint
		spendEvent, err := s.cfg.Notifier.RegisterSpendNtfn(
			&op, st.req.SpentOutputs[i].PkScript,
			st.req.HeightHint,
		)
		if err != nil {
			cancelAll()

			return fmt.Errorf("register spend ntfn for %v: %w",
				op, err)
		}
		cancels = append(cancels, spendEvent.Cancel)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()

			select {
			case spend, ok := <-spendEvent.Spend:
				if !ok {
					return
				}

				select {
				case s.preSignedSpends <- &preSignedSpend{
					txid:  txid,
					spend: spend,
				}:
				case <-s.quit:
				}

			case <-s.quit:
			}
		}()
	}

	st.watching = true
	st.cancelWatch = cancels

	log.Debugf("Watching %d input(s) of pre-signed tx %v from height %d",
		len(cancels), txid, st.req.HeightHint)

	return nil
}

// retryPreSignedTx logs a failed step and leaves the tx to be retried on the
// next block.
func (s *UtxoSweeper) retryPreSignedTx(txid chainhash.Hash,
	st *preSignedTxState, stepErr error) {

	log.Warnf("Pre-signed tx %v: %v, retrying on next block (height=%d, "+
		"deadline=%v)", txid, stepErr, s.currentHeight,
		st.req.DeadlineHeight)
}

// handlePreSignedSpend acts on a confirmed spend of one of the inputs of a
// pre-signed tx. The tx itself as the spender means it confirmed: nothing is
// left to publish, only the anchor still needs to be swept. Any other spender
// means the tx can never confirm, which ends the lifecycle.
func (s *UtxoSweeper) handlePreSignedSpend(ps *preSignedSpend) {
	st, ok := s.preSigned[ps.txid]
	if !ok {
		log.Debugf("Spend of an input of unknown pre-signed tx %v",
			ps.txid)

		return
	}

	spender := *ps.spend.SpenderTxHash
	if spender != ps.txid {
		log.Infof("Input %v of pre-signed tx %v spent by %v at "+
			"height %d, giving up", ps.spend.SpentOutPoint,
			ps.txid, spender, ps.spend.SpendingHeight)

		s.finishPreSignedTx(ps.txid, st, Result{
			Err: fmt.Errorf("%w: spent by %v",
				ErrPreSignedTxConflict, spender),
		})

		return
	}

	log.Infof("Pre-signed tx %v confirmed at height %d", ps.txid,
		ps.spend.SpendingHeight)

	st.confirmed = true
	st.published = true

	// A confirmed tx is past publication. If the anchor is not being
	// swept yet (the publication kept failing, or the tx confirmed while
	// we were offline), register it now.
	s.progressPreSignedTx(ps.txid, st)
}

// handlePreSignedAnchorResult acts on the final result of the anchor sweep
// of a pre-signed tx.
func (s *UtxoSweeper) handlePreSignedAnchorResult(r *preSignedAnchorResult) {
	st, ok := s.preSigned[r.txid]
	if !ok {
		log.Debugf("Anchor result for unknown pre-signed tx %v", r.txid)

		return
	}

	switch {
	// Our child confirmed, which means the parent did too.
	case r.result.Err == nil:
		if r.result.Tx != nil {
			log.Infof("Anchor of pre-signed tx %v swept by tx=%v",
				r.txid, r.result.Tx.TxHash())
		}

		s.finishPreSignedTx(r.txid, st, r.result)

	// Someone else spent the anchor. Once the parent has been confirmed
	// for 16 blocks the output is anyone-can-spend, so this is the
	// expected outcome when our own child never confirmed. Either way
	// CPFP is moot now.
	case errors.Is(r.result.Err, ErrRemoteSpend):
		log.Debugf("Anchor of pre-signed tx %v spent by a third party",
			r.txid)

		s.finishPreSignedTx(r.txid, st, r.result)

	// The anchor sweep gave up. The usual reason is a parent that left
	// the mempool (evicted or replaced), which the child reports as a
	// missing input. Nobody else re-broadcasts the parent and the output
	// it claims is still ours, so go back to the publish step instead of
	// concluding. The fatal anchor input is pruned from the sweeper on
	// the next block, before the lifecycle is progressed again, so the
	// anchor can then be registered afresh. The same holds for any other
	// failure of the child: the lifecycle only ends with a spend.
	default:
		log.Warnf("Anchor sweep of pre-signed tx %v failed: %v, "+
			"going back to publishing the parent", r.txid,
			r.result.Err)

		st.published = false
		st.anchorOffered = false
	}
}

// finishPreSignedTx delivers the final result to every listener and drops the
// tx from the tracked set.
func (s *UtxoSweeper) finishPreSignedTx(txid chainhash.Hash,
	st *preSignedTxState, result Result) {

	for _, cancel := range st.cancelWatch {
		cancel()
	}

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
