package contractcourt

import (
	"encoding/binary"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
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

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// sweepTx will be non-nil if we've already crafted a transaction to
	// sweep a direct HTLC output. This is only a concern if we're sweeping
	// from the commitment transaction of the remote party.
	//
	// TODO(roasbeef): send off to utxobundler
	sweepTx *wire.MsgTx

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

	return h
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) ResolverKey() []byte {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed success tx,
	// otherwise, it's just the ClaimOutpoint.
	var op wire.OutPoint
	if h.htlcResolution.SignedSuccessTx != nil {
		op = h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint
	} else {
		op = h.htlcResolution.ClaimOutpoint
	}

	key := newResolverID(op)
	return key[:]
}

// Resolve attempts to resolve an unresolved incoming HTLC that we know the
// preimage to. If the HTLC is on the commitment of the remote party, then we'll
// simply sweep it directly. Otherwise, we'll hand this off to the utxo nursery
// to do its duty. There is no need to make a call to the invoice registry
// anymore. Every HTLC has already passed through the incoming contest resolver
// and in there the invoice was already marked as settled.
//
// TODO(roasbeef): create multi to batch
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.resolved {
		return nil, nil
	}

	// If we don't have a success transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	if h.htlcResolution.SignedSuccessTx == nil {
		return h.resolveRemoteCommitOutput()
	}

	// Otherwise this an output on our own commitment, and we must start by
	// broadcasting the second-level success transaction.
	secondLevelOutpoint, err := h.broadcastSuccessTx()
	if err != nil {
		return nil, err
	}

	// To wrap this up, we'll wait until the second-level transaction has
	// been spent, then fully resolve the contract.
	log.Infof("%T(%x): waiting for second-level HTLC output to be spent "+
		"after csv_delay=%v", h, h.htlc.RHash[:], h.htlcResolution.CsvDelay)

	spend, err := waitForSpend(
		secondLevelOutpoint,
		h.htlcResolution.SweepSignDesc.Output.PkScript,
		h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return nil, err
	}

	h.reportLock.Lock()
	h.currentReport.RecoveredBalance = h.currentReport.LimboBalance
	h.currentReport.LimboBalance = 0
	h.reportLock.Unlock()

	h.resolved = true
	return nil, h.checkpointClaim(
		spend.SpenderTxHash, channeldb.ResolverOutcomeClaimed,
	)
}

// broadcastSuccessTx handles an HTLC output on our local commitment by
// broadcasting the second-level success transaction. It returns the ultimate
// outpoint of the second-level tx, that we must wait to be spent for the
// resolver to be fully resolved.
func (h *htlcSuccessResolver) broadcastSuccessTx() (*wire.OutPoint, error) {
	// If we have non-nil SignDetails, this means that have a 2nd level
	// HTLC transaction that is signed using sighash SINGLE|ANYONECANPAY
	// (the case for anchor type channels). In this case we can re-sign it
	// and attach fees at will. We let the sweeper handle this job.  We use
	// the checkpointed outputIncubating field to determine if we already
	// swept the HTLC output into the second level transaction.
	if h.htlcResolution.SignDetails != nil {
		return h.broadcastReSignedSuccessTx()
	}

	// Otherwise we'll publish the second-level transaction directly and
	// offer the resolution to the nursery to handle.
	log.Infof("%T(%x): broadcasting second-layer transition tx: %v",
		h, h.htlc.RHash[:], spew.Sdump(h.htlcResolution.SignedSuccessTx))

	// We'll now broadcast the second layer transaction so we can kick off
	// the claiming process.
	//
	// TODO(roasbeef): after changing sighashes send to tx bundler
	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &h.ShortChanID,
	)
	err := h.PublishTx(h.htlcResolution.SignedSuccessTx, label)
	if err != nil {
		return nil, err
	}

	// Otherwise, this is an output on our commitment transaction. In this
	// case, we'll send it to the incubator, but only if we haven't already
	// done so.
	if !h.outputIncubating {
		log.Infof("%T(%x): incubating incoming htlc output",
			h, h.htlc.RHash[:])

		err := h.IncubateOutputs(
			h.ChanPoint, nil, &h.htlcResolution,
			h.broadcastHeight,
		)
		if err != nil {
			return nil, err
		}

		h.outputIncubating = true

		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}
	}

	return &h.htlcResolution.ClaimOutpoint, nil
}

// broadcastReSignedSuccessTx handles the case where we have non-nil
// SignDetails, and offers the second level transaction to the Sweeper, that
// will re-sign it and attach fees at will.
func (h *htlcSuccessResolver) broadcastReSignedSuccessTx() (
	*wire.OutPoint, error) {

	// Keep track of the tx spending the HTLC output on the commitment, as
	// this will be the confirmed second-level tx we'll ultimately sweep.
	var commitSpend *chainntnfs.SpendDetail

	// We will have to let the sweeper re-sign the success tx and wait for
	// it to confirm, if we haven't already.
	isTaproot := txscript.IsPayToTaproot(
		h.htlcResolution.SweepSignDesc.Output.PkScript,
	)
	if !h.outputIncubating {
		log.Infof("%T(%x): offering second-layer transition tx to "+
			"sweeper: %v", h, h.htlc.RHash[:],
			spew.Sdump(h.htlcResolution.SignedSuccessTx))

		var secondLevelInput input.HtlcSecondLevelAnchorInput
		if isTaproot {
			//nolint:lll
			secondLevelInput = input.MakeHtlcSecondLevelSuccessTaprootInput(
				h.htlcResolution.SignedSuccessTx,
				h.htlcResolution.SignDetails, h.htlcResolution.Preimage,
				h.broadcastHeight,
			)
		} else {
			//nolint:lll
			secondLevelInput = input.MakeHtlcSecondLevelSuccessAnchorInput(
				h.htlcResolution.SignedSuccessTx,
				h.htlcResolution.SignDetails, h.htlcResolution.Preimage,
				h.broadcastHeight,
			)
		}

		_, err := h.Sweeper.SweepInput(
			&secondLevelInput,
			sweep.Params{
				Fee: sweep.FeePreference{
					ConfTarget: secondLevelConfTarget,
				},
			},
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%x): waiting for second-level HTLC success "+
			"transaction to confirm", h, h.htlc.RHash[:])

		// Wait for the second level transaction to confirm.
		commitSpend, err = waitForSpend(
			&h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint,
			h.htlcResolution.SignDetails.SignDesc.Output.PkScript,
			h.broadcastHeight, h.Notifier, h.quit,
		)
		if err != nil {
			return nil, err
		}

		// Now that the second-level transaction has confirmed, we
		// checkpoint the state so we'll go to the next stage in case
		// of restarts.
		h.outputIncubating = true
		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return nil, err
		}

		log.Infof("%T(%x): second-level HTLC success transaction "+
			"confirmed!", h, h.htlc.RHash[:])
	}

	// If we ended up here after a restart, we must again get the
	// spend notification.
	if commitSpend == nil {
		var err error
		commitSpend, err = waitForSpend(
			&h.htlcResolution.SignedSuccessTx.TxIn[0].PreviousOutPoint,
			h.htlcResolution.SignDetails.SignDesc.Output.PkScript,
			h.broadcastHeight, h.Notifier, h.quit,
		)
		if err != nil {
			return nil, err
		}
	}

	// The HTLC success tx has a CSV lock that we must wait for, and if
	// this is a lease enforced channel and we're the imitator, we may need
	// to wait for longer.
	waitHeight := h.deriveWaitHeight(
		h.htlcResolution.CsvDelay, commitSpend,
	)

	// Now that the sweeper has broadcasted the second-level transaction,
	// it has confirmed, and we have checkpointed our state, we'll sweep
	// the second level output. We report the resolver has moved the next
	// stage.
	h.reportLock.Lock()
	h.currentReport.Stage = 2
	h.currentReport.MaturityHeight = waitHeight
	h.reportLock.Unlock()

	if h.hasCLTV() {
		log.Infof("%T(%x): waiting for CSV and CLTV lock to "+
			"expire at height %v", h, h.htlc.RHash[:],
			waitHeight)
	} else {
		log.Infof("%T(%x): waiting for CSV lock to expire at "+
			"height %v", h, h.htlc.RHash[:], waitHeight)
	}

	err := waitForHeight(waitHeight, h.Notifier, h.quit)
	if err != nil {
		return nil, err
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

	// Finally, let the sweeper sweep the second-level output.
	log.Infof("%T(%x): CSV lock expired, offering second-layer "+
		"output to sweeper: %v", h, h.htlc.RHash[:], op)

	// Let the sweeper sweep the second-level output now that the
	// CSV/CLTV locks have expired.
	var witType input.StandardWitnessType
	if isTaproot {
		witType = input.TaprootHtlcAcceptedSuccessSecondLevel
	} else {
		witType = input.HtlcAcceptedSuccessSecondLevel
	}
	inp := h.makeSweepInput(
		op, witType,
		input.LeaseHtlcAcceptedSuccessSecondLevel,
		&h.htlcResolution.SweepSignDesc,
		h.htlcResolution.CsvDelay, h.broadcastHeight,
		h.htlc.RHash,
	)
	// TODO(roasbeef): need to update above for leased types
	_, err = h.Sweeper.SweepInput(
		inp,
		sweep.Params{
			Fee: sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			},
		},
	)
	if err != nil {
		return nil, err
	}

	// Will return this outpoint, when this is spent the resolver is fully
	// resolved.
	return op, nil
}

// resolveRemoteCommitOutput handles sweeping an HTLC output on the remote
// commitment with the preimage. In this case we can sweep the output directly,
// and don't have to broadcast a second-level transaction.
func (h *htlcSuccessResolver) resolveRemoteCommitOutput() (
	ContractResolver, error) {

	// If we don't already have the sweep transaction constructed, we'll do
	// so and broadcast it.
	if h.sweepTx == nil {
		log.Infof("%T(%x): crafting sweep tx for incoming+remote "+
			"htlc confirmed", h, h.htlc.RHash[:])

		isTaproot := txscript.IsPayToTaproot(
			h.htlcResolution.SweepSignDesc.Output.PkScript,
		)

		// Before we can craft out sweeping transaction, we need to
		// create an input which contains all the items required to add
		// this input to a sweeping transaction, and generate a
		// witness.
		var inp input.Input
		if isTaproot {
			inp = lnutils.Ptr(input.MakeTaprootHtlcSucceedInput(
				&h.htlcResolution.ClaimOutpoint,
				&h.htlcResolution.SweepSignDesc,
				h.htlcResolution.Preimage[:],
				h.broadcastHeight,
				h.htlcResolution.CsvDelay,
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

		// With the input created, we can now generate the full sweep
		// transaction, that we'll use to move these coins back into
		// the backing wallet.
		//
		// TODO: Set tx lock time to current block height instead of
		// zero. Will be taken care of once sweeper implementation is
		// complete.
		//
		// TODO: Use time-based sweeper and result chan.
		var err error
		h.sweepTx, err = h.Sweeper.CreateSweepTx(
			[]input.Input{inp},
			sweep.FeePreference{
				ConfTarget: sweepConfTarget,
			}, 0,
		)
		if err != nil {
			return nil, err
		}

		log.Infof("%T(%x): crafted sweep tx=%v", h,
			h.htlc.RHash[:], spew.Sdump(h.sweepTx))

		// TODO(halseth): should checkpoint sweep tx to DB? Since after
		// a restart we might create a different tx, that will conflict
		// with the published one.
	}

	// Register the confirmation notification before broadcasting the sweep
	// transaction.
	sweepTXID := h.sweepTx.TxHash()
	sweepScript := h.sweepTx.TxOut[0].PkScript
	confNtfn, err := h.Notifier.RegisterConfirmationsNtfn(
		&sweepTXID, sweepScript, 1, h.broadcastHeight,
	)
	if err != nil {
		return nil, err
	}

	// Regardless of whether an existing transaction was found or newly
	// constructed, we'll broadcast the sweep transaction to the network.
	label := labels.MakeLabel(
		labels.LabelTypeChannelClose, &h.ShortChanID,
	)
	err = h.PublishTx(h.sweepTx, label)
	if err != nil {
		log.Infof("%T(%x): unable to publish tx: %v",
			h, h.htlc.RHash[:], err)
		confNtfn.Cancel()

		return nil, err
	}

	log.Infof("%T(%x): waiting for sweep tx (txid=%v) to be confirmed", h,
		h.htlc.RHash[:], sweepTXID)

	select {
	case _, ok := <-confNtfn.Confirmed:
		if !ok {
			return nil, errResolverShuttingDown
		}

	case <-h.quit:
		return nil, errResolverShuttingDown
	}

	// Once the transaction has received a sufficient number of
	// confirmations, we'll mark ourselves as fully resolved and exit.
	h.resolved = true

	// Checkpoint the resolver, and write the outcome to disk.
	return nil, h.checkpointClaim(
		&sweepTXID,
		channeldb.ResolverOutcomeClaimed,
	)
}

// checkpointClaim checkpoints the success resolver with the reports it needs.
// If this htlc was claimed two stages, it will write reports for both stages,
// otherwise it will just write for the single htlc claim.
func (h *htlcSuccessResolver) checkpointClaim(spendTx *chainhash.Hash,
	outcome channeldb.ResolverOutcome) error {

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
			ResolverOutcome: outcome,
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
	return h.Checkpoint(h, reports...)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) Stop() {
	close(h.quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcSuccessResolver) IsResolved() bool {
	return h.resolved
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
	if err := binary.Write(w, endian, h.resolved); err != nil {
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
	if err := binary.Read(r, endian, &h.resolved); err != nil {
		return nil, err
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

// A compile time assertion to ensure htlcSuccessResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcSuccessResolver)(nil)
