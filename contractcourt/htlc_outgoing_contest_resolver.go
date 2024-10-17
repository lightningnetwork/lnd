package contractcourt

import (
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
)

// htlcOutgoingContestResolver is a ContractResolver that's able to resolve an
// outgoing HTLC that is still contested. An HTLC is still contested, if at the
// time that we broadcast the commitment transaction, it isn't able to be fully
// resolved. In this case, we'll either wait for the HTLC to timeout, or for
// us to learn of the preimage.
type htlcOutgoingContestResolver struct {
	// htlcTimeoutResolver is the inner solver that this resolver may turn
	// into. This only happens if the HTLC expires on-chain.
	*htlcTimeoutResolver
}

// newOutgoingContestResolver instantiates a new outgoing contested htlc
// resolver.
func newOutgoingContestResolver(res lnwallet.OutgoingHtlcResolution,
	broadcastHeight uint32, htlc channeldb.HTLC,
	resCfg ResolverConfig) *htlcOutgoingContestResolver {

	timeout := newTimeoutResolver(
		res, broadcastHeight, htlc, resCfg,
	)

	return &htlcOutgoingContestResolver{
		htlcTimeoutResolver: timeout,
	}
}

// Launch will call the inner resolver's launch method if the expiry height has
// been reached, otherwise it's a no-op.
func (h *htlcOutgoingContestResolver) Launch() error {
	// NOTE: we don't mark this resolver as launched as the inner resolver
	// will set it when it's launched.
	if h.isLaunched() {
		h.log.Tracef("already launched")
		return nil
	}

	h.log.Debugf("launching contest resolver...")

	_, bestHeight, err := h.ChainIO.GetBestBlock()
	if err != nil {
		return err
	}

	if uint32(bestHeight) < h.htlcResolution.Expiry {
		return nil
	}

	// If the current height is >= expiry, then a timeout path spend will
	// be valid to be included in the next block, and we can immediately
	// return the resolver.
	h.log.Infof("expired (height=%v, expiry=%v), transforming into "+
		"timeout resolver and launching it", bestHeight,
		h.htlcResolution.Expiry)

	return h.htlcTimeoutResolver.Launch()
}

// Resolve commences the resolution of this contract. As this contract hasn't
// yet timed out, we'll wait for one of two things to happen
//
//  1. The HTLC expires. In this case, we'll sweep the funds and send a clean
//     up cancel message to outside sub-systems.
//
//  2. The remote party sweeps this HTLC on-chain, in which case we'll add the
//     pre-image to our global cache, then send a clean up settle message
//     backwards.
//
// When either of these two things happens, we'll create a new resolver which
// is able to handle the final resolution of the contract. We're only the pivot
// point.
func (h *htlcOutgoingContestResolver) Resolve() (ContractResolver, error) {
	// If we're already full resolved, then we don't have anything further
	// to do.
	if h.IsResolved() {
		h.log.Errorf("already resolved")
		return nil, nil
	}

	// Otherwise, we'll watch for two external signals to decide if we'll
	// morph into another resolver, or fully resolve the contract.
	//
	// The output we'll be watching for is the *direct* spend from the HTLC
	// output. If this isn't our commitment transaction, it'll be right on
	// the resolution. Otherwise, we fetch this pointer from the input of
	// the time out transaction.
	outPointToWatch, scriptToWatch, err := h.chainDetailsToWatch()
	if err != nil {
		return nil, err
	}

	// First, we'll register for a spend notification for this output. If
	// the remote party sweeps with the pre-image, we'll be notified.
	spendNtfn, err := h.Notifier.RegisterSpendNtfn(
		outPointToWatch, scriptToWatch, h.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	// We'll quickly check to see if the output has already been spent.
	select {
	// If the output has already been spent, then we can stop early and
	// sweep the pre-image from the output.
	case commitSpend, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, errResolverShuttingDown
		}

		return nil, h.claimCleanUp(commitSpend)

	// If it hasn't, then we'll watch for both the expiration, and the
	// sweeping out this output.
	default:
	}

	// If we reach this point, then we can't fully act yet, so we'll await
	// either of our signals triggering: the HTLC expires, or we learn of
	// the preimage.
	blockEpochs, err := h.Notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return nil, err
	}
	defer blockEpochs.Cancel()

	for {
		select {

		// A new block has arrived, we'll check to see if this leads to
		// HTLC expiration.
		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return nil, errResolverShuttingDown
			}

			// If the current height is >= expiry, then a timeout
			// path spend will be valid to be included in the next
			// block, and we can immediately return the resolver.
			//
			// NOTE: when broadcasting this transaction, btcd will
			// check the timelock in `CheckTransactionStandard`,
			// which requires `expiry < currentHeight+1`. If the
			// check doesn't pass, error `transaction is not
			// finalized` will be returned and the broadcast will
			// fail.
			newHeight := uint32(newBlock.Height)
			expiry := h.htlcResolution.Expiry

			// Check if the expiry height is about to be reached.
			// We offer this HTLC one block earlier to make sure
			// when the next block arrives, the sweeper will pick
			// up this input and sweep it immediately. The sweeper
			// will handle the waiting for the one last block till
			// expiry.
			if newHeight >= expiry-1 {
				h.log.Infof("HTLC about to expire "+
					"(height=%v, expiry=%v), transforming "+
					"into timeout resolver", newHeight,
					h.htlcResolution.Expiry)

				return h.htlcTimeoutResolver, nil
			}

		// The output has been spent! This means the preimage has been
		// revealed on-chain.
		case commitSpend, ok := <-spendNtfn.Spend:
			if !ok {
				return nil, errResolverShuttingDown
			}

			// The only way this output can be spent by the remote
			// party is by revealing the preimage. So we'll perform
			// our duties to clean up the contract once it has been
			// claimed.
			return nil, h.claimCleanUp(commitSpend)

		case <-h.quit:
			return nil, errResolverShuttingDown
		}
	}
}

// report returns a report on the resolution state of the contract.
func (h *htlcOutgoingContestResolver) report() *ContractReport {
	// No locking needed as these values are read-only.

	finalAmt := h.htlc.Amt.ToSatoshis()
	if h.htlcResolution.SignedTimeoutTx != nil {
		finalAmt = btcutil.Amount(
			h.htlcResolution.SignedTimeoutTx.TxOut[0].Value,
		)
	}

	return &ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Type:           ReportOutputOutgoingHtlc,
		Amount:         finalAmt,
		MaturityHeight: h.htlcResolution.Expiry,
		LimboBalance:   finalAmt,
		Stage:          1,
	}
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) Stop() {
	h.log.Debugf("stopping...")
	defer h.log.Debugf("stopped")
	close(h.quit)
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcOutgoingContestResolver) Encode(w io.Writer) error {
	return h.htlcTimeoutResolver.Encode(w)
}

// SupplementDeadline does nothing for an incoming htlc resolver.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcOutgoingContestResolver) SupplementDeadline(_ fn.Option[int32]) {
}

// newOutgoingContestResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newOutgoingContestResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcOutgoingContestResolver, error) {

	h := &htlcOutgoingContestResolver{}
	timeoutResolver, err := newTimeoutResolverFromReader(r, resCfg)
	if err != nil {
		return nil, err
	}
	h.htlcTimeoutResolver = timeoutResolver
	return h, nil
}

// A compile time assertion to ensure htlcOutgoingContestResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcOutgoingContestResolver)(nil)
