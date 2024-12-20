package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/labels"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

// htlcSuccessResolver is a resolver that's capable of sweeping an incoming
// HTLC output on-chain. If this is the remote party's commitment, we'll sweep
// it directly from the commitment output *immediately*. If this is our
// commitment, we'll first broadcast the success transaction, then send it to
// the incubator for sweeping. That's it, no need to send any clean up
// messages.
//
// TODO(roasbeef): don't need to broadcast?
type htlcSuccessResolver struct {
	// htlcResolution is the incoming HTLC resolution for this HTLC. It
	// contains everything we need to properly resolve this HTLC.
	htlcResolution lnwallet.IncomingHtlcResolution

	// outputIncubating returns true if we've sent the output to the output
	// incubator (utxo nursery). In case the htlcResolution has non-nil
	// SignDetails, it means we will let the Sweeper handle broadcasting
	// the secondd-level transaction, and sweeping its output. In this case
	// we let this field indicate whether we need to broadcast the
	// second-level tx (false) or if it has confirmed and we must sweep the
	// second-level output (true).
	outputIncubating bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// htlc contains information on the htlc that we are resolving on-chain.
	htlc channeldb.HTLC

	// currentReport stores the current state of the resolver for reporting
	// over the rpc interface. This should only be reported in case we have
	// a non-nil SignDetails on the htlcResolution, otherwise the nursery
	// will produce reports.
	currentReport ContractReport

	// reportLock prevents concurrent access to the resolver report.
	reportLock sync.Mutex

	contractResolverKit

	htlcLeaseResolver
}

// newSuccessResolver instanties a new htlc success resolver.
func newSuccessResolver(res lnwallet.IncomingHtlcResolution,
	broadcastHeight uint32, htlc channeldb.HTLC,
	resCfg ResolverConfig) *htlcSuccessResolver {

	h := &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		htlcResolution:      res,
		broadcastHeight:     broadcastHeight,
		htlc:                htlc,
	}

	h.initReport()
	h.initLogger(fmt.Sprintf("%T(%v)", h, h.outpoint()))

	return h
}

// outpoint returns the outpoint of the HTLC output we're attempting to sweep.
func (h *htlcSuccessResolver) outpoint() wire.OutPoint {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed success tx,
	// otherwise, it's just the ClaimOutpoint.
	if h.htlcResolution.SignedSuccessTx != nil {
		return h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	}

	return h.htlcResolution.ClaimOutpoint
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) ResolverKey() []byte {
	key := newResolverID(h.outpoint())
	return key[:]
}

// Resolve attempts to resolve an unresolved incoming HTLC that we know the
// preimage to. If the HTLC is on the commitment of the remote party, then we'll
// simply sweep it directly. Otherwise, we'll hand this off to the utxo nursery
// to do its duty. There is no need to make a call to the invoice registry
// anymore. Every HTLC has already passed through the incoming contest resolver
// and in there the invoice was already marked as settled.
//
// NOTE: Part of the ContractResolver interface.
//
// TODO(yy): refactor the interface method to return an error only.
func (h *htlcSuccessResolver) Resolve() (ContractResolver, error) {
	var err error

	switch {
	// If we're already resolved, then we can exit early.
	case h.IsResolved():
		h.log.Errorf("already resolved")

	// If this is an output on the remote party's commitment transaction,
	// use the direct-spend path to sweep the htlc.
	case h.isRemoteCommitOutput():
		err = h.resolveRemoteCommitOutput()

	// If this is an output on our commitment transaction using post-anchor
	// channel type, it will be handled by the sweeper.
	case h.isZeroFeeOutput():
		err = h.resolveSuccessTx()

	// If this is an output on our own commitment using pre-anchor channel
	// type, we will publish the success tx and offer the output to the
	// nursery.
	default:
		err = h.resolveLegacySuccessTx()
	}

	return nil, err
}

// resolveRemoteCommitOutput handles sweeping an HTLC output on the remote
// commitment with the preimage. In this case we can sweep the output directly,
// and don't have to broadcast a second-level transaction.
func (h *htlcSuccessResolver) resolveRemoteCommitOutput() error {
	h.log.Info("waiting for direct-preimage spend of the htlc to confirm")

	// Wait for the direct-preimage HTLC sweep tx to confirm.
	//
	// TODO(yy): use the result chan returned from `SweepInput`.
	sweepTxDetails, err := waitForSpend(
		&h.htlcResolution.ClaimOutpoint,
		h.htlcResolution.SweepSignDesc.Output.PkScript,
		h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return err
	}

	// TODO(yy): should also update the `RecoveredBalance` and
	// `LimboBalance` like other paths?

	// Checkpoint the resolver, and write the outcome to disk.
	return h.checkpointClaim(sweepTxDetails.SpenderTxHash)
}

// checkpointClaim checkpoints the success resolver with the reports it needs.
// If this htlc was claimed two stages, it will write reports for both stages,
// otherwise it will just write for the single htlc claim.
func (h *htlcSuccessResolver) checkpointClaim(spendTx *chainhash.Hash) error {
	// Mark the htlc as final settled.
	err := h.ChainArbitratorConfig.PutFinalHtlcOutcome(
		h.ChannelArbitratorConfig.ShortChanID, h.htlc.HtlcIndex, true,
	)
	if err != nil {
		return err
	}

	// Send notification.
	h.ChainArbitratorConfig.HtlcNotifier.NotifyFinalHtlcEvent(
		models.CircuitKey{
			ChanID: h.ShortChanID,
			HtlcID: h.htlc.HtlcIndex,
		},
		channeldb.FinalHtlcInfo{
			Settled:  true,
			Offchain: false,
		},
	)

	// Create a resolver report for claiming of the htlc itself.
	amt := btcutil.Amount(h.htlcResolution.SweepSignDesc.Output.Value)
	reports := []*channeldb.ResolverReport{
		{
			OutPoint:        h.htlcResolution.ClaimOutpoint,
			Amount:          amt,
			ResolverType:    channeldb.ResolverTypeIncomingHtlc,
			ResolverOutcome: channeldb.ResolverOutcomeClaimed,
			SpendTxID:       spendTx,
		},
	}

	// If we have a success tx, we append a report to represent our first
	// stage claim.
	if h.htlcResolution.SignedSuccessTx != nil {
		// If the SignedSuccessTx is not nil, we are claiming the htlc
		// in two stages, so we need to create a report for the first
		// stage transaction as well.
		spendTx := h.htlcResolution.SignedSuccessTx
		spendTxID := spendTx.TxHash()

		report := &channeldb.ResolverReport{
			OutPoint:        spendTx.TxIn[0].PreviousOutPoint,
			Amount:          h.htlc.Amt.ToSatoshis(),
			ResolverType:    channeldb.ResolverTypeIncomingHtlc,
			ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
			SpendTxID:       &spendTxID,
		}
		reports = append(reports, report)
	}

	// Finally, we checkpoint the resolver with our report(s).
	h.markResolved()
	return h.Checkpoint(h, reports...)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Stop() {
	h.log.Debugf("stopping...")
	defer h.log.Debugf("stopped")

	close(h.quit)
}

// report returns a report on the resolution state of the contract.
func (h *htlcSuccessResolver) report() *ContractReport {
	// If the sign details are nil, the report will be created by handled
	// by the nursery.
	if h.htlcResolution.SignDetails == nil {
		return nil
	}

	h.reportLock.Lock()
	defer h.reportLock.Unlock()
	cpy := h.currentReport
	return &cpy
}

func (h *htlcSuccessResolver) initReport() {
	// We create the initial report. This will only be reported for
	// resolvers not handled by the nursery.
	finalAmt := h.htlc.Amt.ToSatoshis()
	if h.htlcResolution.SignedSuccessTx != nil {
		finalAmt = btcutil.Amount(
			h.htlcResolution.SignedSuccessTx.TxOut[0].Value,
		)
	}

	h.currentReport = ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Type:           ReportOutputIncomingHtlc,
		Amount:         finalAmt,
		MaturityHeight: h.htlcResolution.CsvDelay,
		LimboBalance:   finalAmt,
		Stage:          1,
	}
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Encode(w io.Writer) error {
	// First we'll encode our inner HTLC resolution.
	if err := encodeIncomingResolution(w, &h.htlcResolution); err != nil {
		return err
	}

	// Next, we'll write out the fields that are specified to the contract
	// resolver.
	if err := binary.Write(w, endian, h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.IsResolved()); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(h.htlc.RHash[:]); err != nil {
		return err
	}

	// We encode the sign details last for backwards compatibility.
	err := encodeSignDetails(w, h.htlcResolution.SignDetails)
	if err != nil {
		return err
	}

	return nil
}

// newSuccessResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newSuccessResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcSuccessResolver, error) {

	h := &htlcSuccessResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
	}

	// First we'll decode our inner HTLC resolution.
	if err := decodeIncomingResolution(r, &h.htlcResolution); err != nil {
		return nil, err
	}

	// Next, we'll read all the fields that are specified to the contract
	// resolver.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return nil, err
	}

	var resolved bool
	if err := binary.Read(r, endian, &resolved); err != nil {
		return nil, err
	}
	if resolved {
		h.markResolved()
	}

	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, h.htlc.RHash[:]); err != nil {
		return nil, err
	}

	// Sign details is a new field that was added to the htlc resolution,
	// so it is serialized last for backwards compatibility. We try to read
	// it, but don't error out if there are not bytes left.
	signDetails, err := decodeSignDetails(r)
	if err == nil {
		h.htlcResolution.SignDetails = signDetails
	} else if err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}

	h.initReport()
	h.initLogger(fmt.Sprintf("%T(%v)", h, h.outpoint()))

	return h, nil
}

// Supplement adds additional information to the resolver that is required
// before Resolve() is called.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) Supplement(htlc channeldb.HTLC) {
	h.htlc = htlc
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) HtlcPoint() wire.OutPoint {
	return h.htlcResolution.HtlcPoint()
}

// SupplementDeadline does nothing for an incoming htlc resolver.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcSuccessResolver) SupplementDeadline(_ fn.Option[int32]) {
}

// A compile time assertion to ensure htlcSuccessResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcSuccessResolver)(nil)

// isRemoteCommitOutput returns a bool to indicate whether the htlc output is
// on the remote commitment.
func (h *htlcSuccessResolver) isRemoteCommitOutput() bool {
	// If we don't have a success transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	return h.htlcResolution.SignedSuccessTx == nil
}

// isZeroFeeOutput returns a boolean indicating whether the htlc output is from
// a anchor-enabled channel, which uses the sighash SINGLE|ANYONECANPAY.
func (h *htlcSuccessResolver) isZeroFeeOutput() bool {
	// If we have non-nil SignDetails, this means it has a 2nd level HTLC
	// transaction that is signed using sighash SINGLE|ANYONECANPAY (the
	// case for anchor type channels). In this case we can re-sign it and
	// attach fees at will.
	return h.htlcResolution.SignedSuccessTx != nil &&
		h.htlcResolution.SignDetails != nil
}

// isTaproot returns true if the resolver is for a taproot output.
func (h *htlcSuccessResolver) isTaproot() bool {
	return txscript.IsPayToTaproot(
		h.htlcResolution.SweepSignDesc.Output.PkScript,
	)
}

// sweepRemoteCommitOutput creates a sweep request to sweep the HTLC output on
// the remote commitment via the direct preimage-spend.
func (h *htlcSuccessResolver) sweepRemoteCommitOutput() error {
	// Before we can craft out sweeping transaction, we need to create an
	// input which contains all the items required to add this input to a
	// sweeping transaction, and generate a witness.
	var inp input.Input

	if h.isTaproot() {
		inp = lnutils.Ptr(input.MakeTaprootHtlcSucceedInput(
			&h.htlcResolution.ClaimOutpoint,
			&h.htlcResolution.SweepSignDesc,
			h.htlcResolution.Preimage[:],
			h.broadcastHeight,
			h.htlcResolution.CsvDelay,
			input.WithResolutionBlob(
				h.htlcResolution.ResolutionBlob,
			),
		))
	} else {
		inp = lnutils.Ptr(input.MakeHtlcSucceedInput(
			&h.htlcResolution.ClaimOutpoint,
			&h.htlcResolution.SweepSignDesc,
			h.htlcResolution.Preimage[:],
			h.broadcastHeight,
			h.htlcResolution.CsvDelay,
		))
	}

	// Calculate the budget for this sweep.
	budget := calculateBudget(
		btcutil.Amount(inp.SignDesc().Output.Value),
		h.Budget.DeadlineHTLCRatio,
		h.Budget.DeadlineHTLC,
	)

	deadline := fn.Some(int32(h.htlc.RefundTimeout))

	log.Infof("%T(%x): offering direct-preimage HTLC output to sweeper "+
		"with deadline=%v, budget=%v", h, h.htlc.RHash[:],
		h.htlc.RefundTimeout, budget)

	// We'll now offer the direct preimage HTLC to the sweeper.
	_, err := h.Sweeper.SweepInput(
		inp,
		sweep.Params{
			Budget:         budget,
			DeadlineHeight: deadline,
		},
	)

	return err
}

// sweepSuccessTx attempts to sweep the second level success tx.
func (h *htlcSuccessResolver) sweepSuccessTx() error {
	var secondLevelInput input.HtlcSecondLevelAnchorInput
	if h.isTaproot() {
		secondLevelInput = input.MakeHtlcSecondLevelSuccessTaprootInput(
			h.htlcResolution.SignedSuccessTx,
			h.htlcResolution.SignDetails, h.htlcResolution.Preimage,
			h.broadcastHeight, input.WithResolutionBlob(
				h.htlcResolution.ResolutionBlob,
			),
		)
	} else {
		secondLevelInput = input.MakeHtlcSecondLevelSuccessAnchorInput(
			h.htlcResolution.SignedSuccessTx,
			h.htlcResolution.SignDetails, h.htlcResolution.Preimage,
			h.broadcastHeight,
		)
	}

	// Calculate the budget for this sweep.
	value := btcutil.Amount(secondLevelInput.SignDesc().Output.Value)
	budget := calculateBudget(
		value, h.Budget.DeadlineHTLCRatio, h.Budget.DeadlineHTLC,
	)

	// The deadline would be the CLTV in this HTLC output. If we are the
	// initiator of this force close, with the default
	// `IncomingBroadcastDelta`, it means we have 10 blocks left when going
	// onchain.
	deadline := fn.Some(int32(h.htlc.RefundTimeout))

	h.log.Infof("offering second-level HTLC success tx to sweeper with "+
		"deadline=%v, budget=%v", h.htlc.RefundTimeout, budget)

	// We'll now offer the second-level transaction to the sweeper.
	_, err := h.Sweeper.SweepInput(
		&secondLevelInput,
		sweep.Params{
			Budget:         budget,
			DeadlineHeight: deadline,
		},
	)

	return err
}

// sweepSuccessTxOutput attempts to sweep the output of the second level
// success tx.
func (h *htlcSuccessResolver) sweepSuccessTxOutput() error {
	h.log.Debugf("sweeping output %v from 2nd-level HTLC success tx",
		h.htlcResolution.ClaimOutpoint)

	// This should be non-blocking as we will only attempt to sweep the
	// output when the second level tx has already been confirmed. In other
	// words, waitForSpend will return immediately.
	commitSpend, err := waitForSpend(
		&h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint,
		h.htlcResolution.SignDetails.SignDesc.Output.PkScript,
		h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return err
	}

	// The HTLC success tx has a CSV lock that we must wait for, and if
	// this is a lease enforced channel and we're the imitator, we may need
	// to wait for longer.
	waitHeight := h.deriveWaitHeight(h.htlcResolution.CsvDelay, commitSpend)

	// Now that the sweeper has broadcasted the second-level transaction,
	// it has confirmed, and we have checkpointed our state, we'll sweep
	// the second level output. We report the resolver has moved the next
	// stage.
	h.reportLock.Lock()
	h.currentReport.Stage = 2
	h.currentReport.MaturityHeight = waitHeight
	h.reportLock.Unlock()

	if h.hasCLTV() {
		log.Infof("%T(%x): waiting for CSV and CLTV lock to expire at "+
			"height %v", h, h.htlc.RHash[:], waitHeight)
	} else {
		log.Infof("%T(%x): waiting for CSV lock to expire at height %v",
			h, h.htlc.RHash[:], waitHeight)
	}

	// We'll use this input index to determine the second-level output
	// index on the transaction, as the signatures requires the indexes to
	// be the same. We don't look for the second-level output script
	// directly, as there might be more than one HTLC output to the same
	// pkScript.
	op := &wire.OutPoint{
		Hash:  *commitSpend.SpenderTxHash,
		Index: commitSpend.SpenderInputIndex,
	}

	// Let the sweeper sweep the second-level output now that the
	// CSV/CLTV locks have expired.
	var witType input.StandardWitnessType
	if h.isTaproot() {
		witType = input.TaprootHtlcAcceptedSuccessSecondLevel
	} else {
		witType = input.HtlcAcceptedSuccessSecondLevel
	}
	inp := h.makeSweepInput(
		op, witType,
		input.LeaseHtlcAcceptedSuccessSecondLevel,
		&h.htlcResolution.SweepSignDesc,
		h.htlcResolution.CsvDelay, uint32(commitSpend.SpendingHeight),
		h.htlc.RHash, h.htlcResolution.ResolutionBlob,
	)

	// Calculate the budget for this sweep.
	budget := calculateBudget(
		btcutil.Amount(inp.SignDesc().Output.Value),
		h.Budget.NoDeadlineHTLCRatio,
		h.Budget.NoDeadlineHTLC,
	)

	log.Infof("%T(%x): offering second-level success tx output to sweeper "+
		"with no deadline and budget=%v at height=%v", h,
		h.htlc.RHash[:], budget, waitHeight)

	// TODO(yy): use the result chan returned from SweepInput.
	_, err = h.Sweeper.SweepInput(
		inp,
		sweep.Params{
			Budget: budget,

			// For second level success tx, there's no rush to get
			// it confirmed, so we use a nil deadline.
			DeadlineHeight: fn.None[int32](),
		},
	)

	return err
}

// resolveLegacySuccessTx handles an HTLC output from a pre-anchor type channel
// by broadcasting the second-level success transaction.
func (h *htlcSuccessResolver) resolveLegacySuccessTx() error {
	// Otherwise we'll publish the second-level transaction directly and
	// offer the resolution to the nursery to handle.
	h.log.Infof("broadcasting legacy second-level success tx: %v",
		h.htlcResolution.SignedSuccessTx.TxHash())

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process.
	//
	// TODO(yy): offer it to the sweeper instead.
	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &h.ShortChanID,
	)
	err := h.PublishTx(h.htlcResolution.SignedSuccessTx, label)
	if err != nil {
		return err
	}

	// Fast-forward to resolve the output from the success tx if the it has
	// already been sent to the UtxoNursery.
	if h.outputIncubating {
		return h.resolveSuccessTxOutput(h.htlcResolution.ClaimOutpoint)
	}

	h.log.Infof("incubating incoming htlc output")

	// Send the output to the incubator.
	err = h.IncubateOutputs(
		h.ChanPoint, fn.None[lnwallet.OutgoingHtlcResolution](),
		fn.Some(h.htlcResolution),
		h.broadcastHeight, fn.Some(int32(h.htlc.RefundTimeout)),
	)
	if err != nil {
		return err
	}

	// Mark the output as incubating and checkpoint it.
	h.outputIncubating = true
	if err := h.Checkpoint(h); err != nil {
		return err
	}

	// Move to resolve the output.
	return h.resolveSuccessTxOutput(h.htlcResolution.ClaimOutpoint)
}

// resolveSuccessTx waits for the sweeping tx of the second-level success tx to
// confirm and offers the output from the success tx to the sweeper.
func (h *htlcSuccessResolver) resolveSuccessTx() error {
	h.log.Infof("waiting for 2nd-level HTLC success transaction to confirm")

	// Create aliases to make the code more readable.
	outpoint := h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	pkScript := h.htlcResolution.SignDetails.SignDesc.Output.PkScript

	// Wait for the second level transaction to confirm.
	commitSpend, err := waitForSpend(
		&outpoint, pkScript, h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return err
	}

	// We'll use this input index to determine the second-level output
	// index on the transaction, as the signatures requires the indexes to
	// be the same. We don't look for the second-level output script
	// directly, as there might be more than one HTLC output to the same
	// pkScript.
	op := wire.OutPoint{
		Hash:  *commitSpend.SpenderTxHash,
		Index: commitSpend.SpenderInputIndex,
	}

	// If the 2nd-stage sweeping has already been started, we can
	// fast-forward to start the resolving process for the stage two
	// output.
	if h.outputIncubating {
		return h.resolveSuccessTxOutput(op)
	}

	// Now that the second-level transaction has confirmed, we checkpoint
	// the state so we'll go to the next stage in case of restarts.
	h.outputIncubating = true
	if err := h.Checkpoint(h); err != nil {
		log.Errorf("unable to Checkpoint: %v", err)
		return err
	}

	h.log.Infof("2nd-level HTLC success tx=%v confirmed",
		commitSpend.SpenderTxHash)

	// Send the sweep request for the output from the success tx.
	if err := h.sweepSuccessTxOutput(); err != nil {
		return err
	}

	return h.resolveSuccessTxOutput(op)
}

// resolveSuccessTxOutput waits for the spend of the output from the 2nd-level
// success tx.
func (h *htlcSuccessResolver) resolveSuccessTxOutput(op wire.OutPoint) error {
	// To wrap this up, we'll wait until the second-level transaction has
	// been spent, then fully resolve the contract.
	log.Infof("%T(%x): waiting for second-level HTLC output to be spent "+
		"after csv_delay=%v", h, h.htlc.RHash[:],
		h.htlcResolution.CsvDelay)

	spend, err := waitForSpend(
		&op, h.htlcResolution.SweepSignDesc.Output.PkScript,
		h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return err
	}

	h.reportLock.Lock()
	h.currentReport.RecoveredBalance = h.currentReport.LimboBalance
	h.currentReport.LimboBalance = 0
	h.reportLock.Unlock()

	return h.checkpointClaim(spend.SpenderTxHash)
}

// Launch creates an input based on the details of the incoming htlc resolution
// and offers it to the sweeper.
func (h *htlcSuccessResolver) Launch() error {
	if h.isLaunched() {
		h.log.Tracef("already launched")
		return nil
	}

	h.log.Debugf("launching resolver...")
	h.markLaunched()

	switch {
	// If we're already resolved, then we can exit early.
	case h.IsResolved():
		h.log.Errorf("already resolved")
		return nil

	// If this is an output on the remote party's commitment transaction,
	// use the direct-spend path.
	case h.isRemoteCommitOutput():
		return h.sweepRemoteCommitOutput()

	// If this is an anchor type channel, we now sweep either the
	// second-level success tx or the output from the second-level success
	// tx.
	case h.isZeroFeeOutput():
		// If the second-level success tx has already been swept, we
		// can go ahead and sweep its output.
		if h.outputIncubating {
			return h.sweepSuccessTxOutput()
		}

		// Otherwise, sweep the second level tx.
		return h.sweepSuccessTx()

	// If this is a legacy channel type, the output is handled by the
	// nursery via the Resolve so we do nothing here.
	//
	// TODO(yy): handle the legacy output by offering it to the sweeper.
	default:
		return nil
	}
}
