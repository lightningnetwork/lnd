package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sweep"
)

// htlcTimeoutResolver is a ContractResolver that's capable of resolving an
// outgoing HTLC. The HTLC may be on our commitment transaction, or on the
// commitment transaction of the remote party. An output on our commitment
// transaction is considered fully resolved once the second-level transaction
// has been confirmed (and reached a sufficient depth). An output on the
// commitment transaction of the remote party is resolved once we detect a
// spend of the direct HTLC output using the timeout clause.
type htlcTimeoutResolver struct {
	// htlcResolution contains all the information required to properly
	// resolve this outgoing HTLC.
	htlcResolution lnwallet.OutgoingHtlcResolution

	// outputIncubating returns true if we've sent the output to the output
	// incubator (utxo nursery).
	outputIncubating bool

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	//
	// TODO(roasbeef): wrap above into definite resolution embedding?
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

// newTimeoutResolver instantiates a new timeout htlc resolver.
func newTimeoutResolver(res lnwallet.OutgoingHtlcResolution,
	broadcastHeight uint32, htlc channeldb.HTLC,
	resCfg ResolverConfig) *htlcTimeoutResolver {

	h := &htlcTimeoutResolver{
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
func (h *htlcTimeoutResolver) ResolverKey() []byte {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed timeout tx,
	// otherwise, it's just the ClaimOutpoint.
	var op wire.OutPoint
	if h.htlcResolution.SignedTimeoutTx != nil {
		op = h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	} else {
		op = h.htlcResolution.ClaimOutpoint
	}

	key := newResolverID(op)
	return key[:]
}

const (
	// expectedRemoteWitnessSuccessSize is the expected size of the witness
	// on the remote commitment transaction for an outgoing HTLC that is
	// swept on-chain by them with pre-image.
	expectedRemoteWitnessSuccessSize = 5

	// remotePreimageIndex index within the witness on the remote
	// commitment transaction that will hold they pre-image if they go to
	// sweep it on chain.
	remotePreimageIndex = 3

	// localPreimageIndex is the index within the witness on the local
	// commitment transaction for an outgoing HTLC that will hold the
	// pre-image if the remote party sweeps it.
	localPreimageIndex = 1
)

// claimCleanUp is a helper method that's called once the HTLC output is spent
// by the remote party. It'll extract the preimage, add it to the global cache,
// and finally send the appropriate clean up message.
func (h *htlcTimeoutResolver) claimCleanUp(
	commitSpend *chainntnfs.SpendDetail) (ContractResolver, error) {

	// Depending on if this is our commitment or not, then we'll be looking
	// for a different witness pattern.
	spenderIndex := commitSpend.SpenderInputIndex
	spendingInput := commitSpend.SpendingTx.TxIn[spenderIndex]

	log.Infof("%T(%v): extracting preimage! remote party spent "+
		"HTLC with tx=%v", h, h.htlcResolution.ClaimOutpoint,
		spew.Sdump(commitSpend.SpendingTx))

	// If this is the remote party's commitment, then we'll be looking for
	// them to spend using the second-level success transaction.
	var preimageBytes []byte
	if h.htlcResolution.SignedTimeoutTx == nil {
		// The witness stack when the remote party sweeps the output to
		// them looks like:
		//
		//  * <0> <sender sig> <recvr sig> <preimage> <witness script>
		preimageBytes = spendingInput.Witness[remotePreimageIndex]
	} else {
		// Otherwise, they'll be spending directly from our commitment
		// output. In which case the witness stack looks like:
		//
		//  * <sig> <preimage> <witness script>
		preimageBytes = spendingInput.Witness[localPreimageIndex]
	}

	preimage, err := lntypes.MakePreimage(preimageBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to create pre-image from "+
			"witness: %v", err)
	}

	log.Infof("%T(%v): extracting preimage=%v from on-chain "+
		"spend!", h, h.htlcResolution.ClaimOutpoint, preimage)

	// With the preimage obtained, we can now add it to the global cache.
	if err := h.PreimageDB.AddPreimages(preimage); err != nil {
		log.Errorf("%T(%v): unable to add witness to cache",
			h, h.htlcResolution.ClaimOutpoint)
	}

	var pre [32]byte
	copy(pre[:], preimage[:])

	// Finally, we'll send the clean up message, mark ourselves as
	// resolved, then exit.
	if err := h.DeliverResolutionMsg(ResolutionMsg{
		SourceChan: h.ShortChanID,
		HtlcIndex:  h.htlc.HtlcIndex,
		PreImage:   &pre,
	}); err != nil {
		return nil, err
	}
	h.resolved = true

	// Checkpoint our resolver with a report which reflects the preimage
	// claim by the remote party.
	amt := btcutil.Amount(h.htlcResolution.SweepSignDesc.Output.Value)
	report := &channeldb.ResolverReport{
		OutPoint:        h.htlcResolution.ClaimOutpoint,
		Amount:          amt,
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeClaimed,
		SpendTxID:       commitSpend.SpenderTxHash,
	}

	return nil, h.Checkpoint(h, report)
}

// chainDetailsToWatch returns the output and script which we use to watch for
// spends from the direct HTLC output on the commitment transaction.
func (h *htlcTimeoutResolver) chainDetailsToWatch() (*wire.OutPoint, []byte, error) {
	// If there's no timeout transaction, it means we are spending from a
	// remote commit, then the claim output is the output directly on the
	// commitment transaction, so we'll just use that.
	if h.htlcResolution.SignedTimeoutTx == nil {
		outPointToWatch := h.htlcResolution.ClaimOutpoint
		scriptToWatch := h.htlcResolution.SweepSignDesc.Output.PkScript

		return &outPointToWatch, scriptToWatch, nil
	}

	// If SignedTimeoutTx is not nil, this is the local party's commitment,
	// and we'll need to grab watch the output that our timeout transaction
	// points to. We can directly grab the outpoint, then also extract the
	// witness script (the last element of the witness stack) to
	// re-construct the pkScript we need to watch.
	outPointToWatch := h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	witness := h.htlcResolution.SignedTimeoutTx.TxIn[0].Witness
	scriptToWatch, err := input.WitnessScriptHash(witness[len(witness)-1])
	if err != nil {
		return nil, nil, err
	}

	return &outPointToWatch, scriptToWatch, nil
}

// isPreimageSpend returns true if the passed spend on the specified commitment
// is a success spend that reveals the pre-image or not.
func isPreimageSpend(spend *chainntnfs.SpendDetail, localCommit bool) bool {
	// Based on the spending input index and transaction, obtain the
	// witness that tells us what type of spend this is.
	spenderIndex := spend.SpenderInputIndex
	spendingInput := spend.SpendingTx.TxIn[spenderIndex]
	spendingWitness := spendingInput.Witness

	// If this is the remote commitment then the only possible spends for
	// outgoing HTLCs are:
	//
	//  RECVR: <0> <sender sig> <recvr sig> <preimage> (2nd level success spend)
	//  REVOK: <sig> <key>
	//  SENDR: <sig> 0
	//
	// In this case, if 5 witness elements are present (factoring the
	// witness script), and the 3rd element is the size of the pre-image,
	// then this is a remote spend. If not, then we swept it ourselves, or
	// revoked their output.
	if !localCommit {
		return len(spendingWitness) == expectedRemoteWitnessSuccessSize &&
			len(spendingWitness[remotePreimageIndex]) == lntypes.HashSize
	}

	// Otherwise, for our commitment, the only possible spends for an
	// outgoing HTLC are:
	//
	//  SENDR: <0> <sendr sig>  <recvr sig> <0> (2nd level timeout)
	//  RECVR: <recvr sig>  <preimage>
	//  REVOK: <revoke sig> <revoke key>
	//
	// So the only success case has the pre-image as the 2nd (index 1)
	// element in the witness.
	return len(spendingWitness[localPreimageIndex]) == lntypes.HashSize
}

// Resolve kicks off full resolution of an outgoing HTLC output. If it's our
// commitment, it isn't resolved until we see the second level HTLC txn
// confirmed. If it's the remote party's commitment, we don't resolve until we
// see a direct sweep via the timeout clause.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.resolved {
		return nil, nil
	}

	// Start by spending the HTLC output, either by broadcasting the
	// second-level timeout transaction, or directly if this is the remote
	// commitment.
	commitSpend, err := h.spendHtlcOutput()
	if err != nil {
		return nil, err
	}

	// If the spend reveals the pre-image, then we'll enter the clean up
	// workflow to pass the pre-image back to the incoming link, add it to
	// the witness cache, and exit.
	if isPreimageSpend(
		commitSpend, h.htlcResolution.SignedTimeoutTx != nil,
	) {

		log.Infof("%T(%v): HTLC has been swept with pre-image by "+
			"remote party during timeout flow! Adding pre-image to "+
			"witness cache", h.htlcResolution.ClaimOutpoint)

		return h.claimCleanUp(commitSpend)
	}

	log.Infof("%T(%v): resolving htlc with incoming fail msg, fully "+
		"confirmed", h, h.htlcResolution.ClaimOutpoint)

	// At this point, the second-level transaction is sufficiently
	// confirmed, or a transaction directly spending the output is.
	// Therefore, we can now send back our clean up message, failing the
	// HTLC on the incoming link.
	failureMsg := &lnwire.FailPermanentChannelFailure{}
	if err := h.DeliverResolutionMsg(ResolutionMsg{
		SourceChan: h.ShortChanID,
		HtlcIndex:  h.htlc.HtlcIndex,
		Failure:    failureMsg,
	}); err != nil {
		return nil, err
	}

	// Depending on whether this was a local or remote commit, we must
	// handle the spending transaction accordingly.
	return h.handleCommitSpend(commitSpend)
}

// sweepSecondLevelTx sends a second level timeout transaction to the sweeper.
// This transaction uses the SINLGE|ANYONECANPAY flag.
func (h *htlcTimeoutResolver) sweepSecondLevelTx() error {
	log.Infof("%T(%x): offering second-layer timeout tx to sweeper: %v",
		h, h.htlc.RHash[:],
		spew.Sdump(h.htlcResolution.SignedTimeoutTx))

	inp := input.MakeHtlcSecondLevelTimeoutAnchorInput(
		h.htlcResolution.SignedTimeoutTx,
		h.htlcResolution.SignDetails,
		h.broadcastHeight,
	)
	_, err := h.Sweeper.SweepInput(
		&inp, sweep.Params{
			Fee: sweep.FeePreference{
				ConfTarget: secondLevelConfTarget,
			},
		},
	)

	// TODO(yy): checkpoint here?
	return err
}

// sendSecondLevelTxLegacy sends a second level timeout transaction to the utxo
// nursery. This transaction uses the legacy SIGHASH_ALL flag.
func (h *htlcTimeoutResolver) sendSecondLevelTxLegacy() error {
	log.Debugf("%T(%v): incubating htlc output", h,
		h.htlcResolution.ClaimOutpoint)

	err := h.IncubateOutputs(
		h.ChanPoint, &h.htlcResolution, nil,
		h.broadcastHeight,
	)
	if err != nil {
		return err
	}

	h.outputIncubating = true

	return h.Checkpoint(h)
}

// spendHtlcOutput handles the initial spend of an HTLC output via the timeout
// clause. If this is our local commitment, the second-level timeout TX will be
// used to spend the output into the next stage. If this is the remote
// commitment, the output will be swept directly without the timeout
// transaction.
func (h *htlcTimeoutResolver) spendHtlcOutput() (*chainntnfs.SpendDetail, error) {
	switch {
	// If we have non-nil SignDetails, this means that have a 2nd level
	// HTLC transaction that is signed using sighash SINGLE|ANYONECANPAY
	// (the case for anchor type channels). In this case we can re-sign it
	// and attach fees at will. We let the sweeper handle this job.
	case h.htlcResolution.SignDetails != nil && !h.outputIncubating:
		if err := h.sweepSecondLevelTx(); err != nil {
			log.Errorf("Sending timeout tx to sweeper: %v", err)
			return nil, err
		}

	// If we have no SignDetails, and we haven't already sent the output to
	// the utxo nursery, then we'll do so now.
	case h.htlcResolution.SignDetails == nil && !h.outputIncubating:
		if err := h.sendSecondLevelTxLegacy(); err != nil {
			log.Errorf("Sending timeout tx to nursery: %v", err)
			return nil, err
		}
	}

	// Now that we've handed off the HTLC to the nursery or sweeper, we'll
	// watch for a spend of the output, and make our next move off of that.
	// Depending on if this is our commitment, or the remote party's
	// commitment, we'll be watching a different outpoint and script.
	return h.watchHtlcSpend()
}

// watchHtlcSpend watches for a spend of the HTLC output. For neutrino backend,
// it will check blocks for the confirmed spend. For btcd and bitcoind, it will
// check both the mempool and the blocks.
func (h *htlcTimeoutResolver) watchHtlcSpend() (*chainntnfs.SpendDetail,
	error) {

	// TODO(yy): outpointToWatch is always h.HtlcOutpoint(), can refactor
	// to remove the redundancy.
	outpointToWatch, scriptToWatch, err := h.chainDetailsToWatch()
	if err != nil {
		return nil, err
	}

	// If there's no mempool configured, which is the case for SPV node
	// such as neutrino, then we will watch for confirmed spend only.
	if h.Mempool == nil {
		return h.waitForConfirmedSpend(outpointToWatch, scriptToWatch)
	}

	// Watch for a spend of the HTLC output in both the mempool and blocks.
	return h.waitForMempoolOrBlockSpend(*outpointToWatch, scriptToWatch)
}

// waitForConfirmedSpend waits for the HTLC output to be spent and confirmed in
// a block, returns the spend details.
func (h *htlcTimeoutResolver) waitForConfirmedSpend(op *wire.OutPoint,
	pkScript []byte) (*chainntnfs.SpendDetail, error) {

	log.Infof("%T(%v): waiting for spent of HTLC output %v to be "+
		"fully confirmed", h, h.htlcResolution.ClaimOutpoint, op)

	// We'll block here until either we exit, or the HTLC output on the
	// commitment transaction has been spent.
	spend, err := waitForSpend(
		op, pkScript, h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return nil, err
	}

	// Once confirmed, persist the state on disk.
	if err := h.checkPointSecondLevelTx(); err != nil {
		return nil, err
	}

	return spend, err
}

// checkPointSecondLevelTx persists the state of a second level HTLC tx to disk
// if it's published by the sweeper.
func (h *htlcTimeoutResolver) checkPointSecondLevelTx() error {
	// If this was the second level transaction published by the sweeper,
	// we can checkpoint the resolver now that it's confirmed.
	if h.htlcResolution.SignDetails != nil && !h.outputIncubating {
		h.outputIncubating = true
		if err := h.Checkpoint(h); err != nil {
			log.Errorf("unable to Checkpoint: %v", err)
			return err
		}
	}

	return nil
}

// handleCommitSpend handles the spend of the HTLC output on the commitment
// transaction. If this was our local commitment, the spend will be he
// confirmed second-level timeout transaction, and we'll sweep that into our
// wallet. If the was a remote commitment, the resolver will resolve
// immetiately.
func (h *htlcTimeoutResolver) handleCommitSpend(
	commitSpend *chainntnfs.SpendDetail) (ContractResolver, error) {

	var (
		// claimOutpoint will be the outpoint of the second level
		// transaction, or on the remote commitment directly. It will
		// start out as set in the resolution, but we'll update it if
		// the second-level goes through the sweeper and changes its
		// txid.
		claimOutpoint = h.htlcResolution.ClaimOutpoint

		// spendTxID will be the ultimate spend of the claimOutpoint.
		// We set it to the commit spend for now, as this is the
		// ultimate spend in case this is a remote commitment. If we go
		// through the second-level transaction, we'll update this
		// accordingly.
		spendTxID = commitSpend.SpenderTxHash

		reports []*channeldb.ResolverReport
	)

	switch {
	// If the sweeper is handling the second level transaction, wait for
	// the CSV and possible CLTV lock to expire, before sweeping the output
	// on the second-level.
	case h.htlcResolution.SignDetails != nil:
		waitHeight := h.deriveWaitHeight(
			h.htlcResolution.CsvDelay, commitSpend,
		)

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

		// We'll use this input index to determine the second-level
		// output index on the transaction, as the signatures requires
		// the indexes to be the same. We don't look for the
		// second-level output script directly, as there might be more
		// than one HTLC output to the same pkScript.
		op := &wire.OutPoint{
			Hash:  *commitSpend.SpenderTxHash,
			Index: commitSpend.SpenderInputIndex,
		}

		// Let the sweeper sweep the second-level output now that the
		// CSV/CLTV locks have expired.
		inp := h.makeSweepInput(
			op, input.HtlcOfferedTimeoutSecondLevel,
			input.LeaseHtlcOfferedTimeoutSecondLevel,
			&h.htlcResolution.SweepSignDesc,
			h.htlcResolution.CsvDelay, h.broadcastHeight,
			h.htlc.RHash,
		)
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

		// Update the claim outpoint to point to the second-level
		// transaction created by the sweeper.
		claimOutpoint = *op
		fallthrough

	// Finally, if this was an output on our commitment transaction, we'll
	// wait for the second-level HTLC output to be spent, and for that
	// transaction itself to confirm.
	case h.htlcResolution.SignedTimeoutTx != nil:
		log.Infof("%T(%v): waiting for nursery/sweeper to spend CSV "+
			"delayed output", h, claimOutpoint)
		sweepTx, err := waitForSpend(
			&claimOutpoint,
			h.htlcResolution.SweepSignDesc.Output.PkScript,
			h.broadcastHeight, h.Notifier, h.quit,
		)
		if err != nil {
			return nil, err
		}

		// Update the spend txid to the hash of the sweep transaction.
		spendTxID = sweepTx.SpenderTxHash

		// Once our sweep of the timeout tx has confirmed, we add a
		// resolution for our timeoutTx tx first stage transaction.
		timeoutTx := commitSpend.SpendingTx
		index := commitSpend.SpenderInputIndex
		spendHash := commitSpend.SpenderTxHash

		reports = append(reports, &channeldb.ResolverReport{
			OutPoint:        timeoutTx.TxIn[index].PreviousOutPoint,
			Amount:          h.htlc.Amt.ToSatoshis(),
			ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
			ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
			SpendTxID:       spendHash,
		})
	}

	// With the clean up message sent, we'll now mark the contract
	// resolved, update the recovered balance, record the timeout and the
	// sweep txid on disk, and wait.
	h.resolved = true
	h.reportLock.Lock()
	h.currentReport.RecoveredBalance = h.currentReport.LimboBalance
	h.currentReport.LimboBalance = 0
	h.reportLock.Unlock()

	amt := btcutil.Amount(h.htlcResolution.SweepSignDesc.Output.Value)
	reports = append(reports, &channeldb.ResolverReport{
		OutPoint:        claimOutpoint,
		Amount:          amt,
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       spendTxID,
	})

	return nil, h.Checkpoint(h, reports...)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Stop() {
	close(h.quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) IsResolved() bool {
	return h.resolved
}

// report returns a report on the resolution state of the contract.
func (h *htlcTimeoutResolver) report() *ContractReport {
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

func (h *htlcTimeoutResolver) initReport() {
	// We create the initial report. This will only be reported for
	// resolvers not handled by the nursery.
	finalAmt := h.htlc.Amt.ToSatoshis()
	if h.htlcResolution.SignedTimeoutTx != nil {
		finalAmt = btcutil.Amount(
			h.htlcResolution.SignedTimeoutTx.TxOut[0].Value,
		)
	}

	h.currentReport = ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Type:           ReportOutputOutgoingHtlc,
		Amount:         finalAmt,
		MaturityHeight: h.htlcResolution.Expiry,
		LimboBalance:   finalAmt,
		Stage:          1,
	}
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Encode(w io.Writer) error {
	// First, we'll write out the relevant fields of the
	// OutgoingHtlcResolution to the writer.
	if err := encodeOutgoingResolution(w, &h.htlcResolution); err != nil {
		return err
	}

	// With that portion written, we can now write out the fields specific
	// to the resolver itself.
	if err := binary.Write(w, endian, h.outputIncubating); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, h.broadcastHeight); err != nil {
		return err
	}

	if err := binary.Write(w, endian, h.htlc.HtlcIndex); err != nil {
		return err
	}

	// We encode the sign details last for backwards compatibility.
	err := encodeSignDetails(w, h.htlcResolution.SignDetails)
	if err != nil {
		return err
	}

	return nil
}

// newTimeoutResolverFromReader attempts to decode an encoded ContractResolver
// from the passed Reader instance, returning an active ContractResolver
// instance.
func newTimeoutResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*htlcTimeoutResolver, error) {

	h := &htlcTimeoutResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
	}

	// First, we'll read out all the mandatory fields of the
	// OutgoingHtlcResolution that we store.
	if err := decodeOutgoingResolution(r, &h.htlcResolution); err != nil {
		return nil, err
	}

	// With those fields read, we can now read back the fields that are
	// specific to the resolver itself.
	if err := binary.Read(r, endian, &h.outputIncubating); err != nil {
		return nil, err
	}
	if err := binary.Read(r, endian, &h.resolved); err != nil {
		return nil, err
	}
	if err := binary.Read(r, endian, &h.broadcastHeight); err != nil {
		return nil, err
	}

	if err := binary.Read(r, endian, &h.htlc.HtlcIndex); err != nil {
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
func (h *htlcTimeoutResolver) Supplement(htlc channeldb.HTLC) {
	h.htlc = htlc
}

// HtlcPoint returns the htlc's outpoint on the commitment tx.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcTimeoutResolver) HtlcPoint() wire.OutPoint {
	return h.htlcResolution.HtlcPoint()
}

// A compile time assertion to ensure htlcTimeoutResolver meets the
// ContractResolver interface.
var _ htlcContractResolver = (*htlcTimeoutResolver)(nil)

// spendResult is used to hold the result of a spend event from either a
// mempool spend or a block spend.
type spendResult struct {
	// spend contains the details of the spend.
	spend *chainntnfs.SpendDetail

	// err is the error that occurred during the spend notification.
	err error
}

// waitForMempoolOrBlockSpend waits for the htlc output to be spent by a
// transaction that's either be found in the mempool or in a block.
func (h *htlcTimeoutResolver) waitForMempoolOrBlockSpend(op wire.OutPoint,
	pkScript []byte) (*chainntnfs.SpendDetail, error) {

	log.Infof("%T(%v): waiting for spent of HTLC output %v to be found "+
		"in mempool or block", h, h.htlcResolution.ClaimOutpoint, op)

	// Subscribe for block spent(confirmed).
	blockSpent, err := h.Notifier.RegisterSpendNtfn(
		&op, pkScript, h.broadcastHeight,
	)
	if err != nil {
		return nil, fmt.Errorf("register spend: %w", err)
	}

	// Subscribe for mempool spent(unconfirmed).
	mempoolSpent, err := h.Mempool.SubscribeMempoolSpent(op)
	if err != nil {
		return nil, fmt.Errorf("register mempool spend: %w", err)
	}

	// Create a result chan that will be used to receive the spending
	// events.
	result := make(chan *spendResult, 2)

	// Create a goroutine that will wait for either a mempool spend or a
	// block spend.
	//
	// NOTE: no need to use waitgroup here as when the resolver exits, the
	// goroutine will return on the quit channel.
	go h.consumeSpendEvents(result, blockSpent.Spend, mempoolSpent.Spend)

	// Wait for the spend event to be received.
	select {
	case event := <-result:
		// Cancel the mempool subscription as we don't need it anymore.
		h.Mempool.CancelMempoolSpendEvent(mempoolSpent)

		return event.spend, event.err

	case <-h.quit:
		return nil, errResolverShuttingDown
	}
}

// consumeSpendEvents consumes the spend events from the block and mempool
// subscriptions. It exits when a spend event is received from the block, or
// the resolver itself quits. When a spend event is received from the mempool,
// however, it won't exit but continuing to wait for a spend event from the
// block subscription.
//
// NOTE: there could be a case where we found the preimage in the mempool,
// which will be added to our preimage beacon and settle the incoming link,
// meanwhile the timeout sweep tx confirms. This outgoing HTLC is "free" money
// and is not swept here.
//
// TODO(yy): sweep the outgoing htlc if it's confirmed.
func (h *htlcTimeoutResolver) consumeSpendEvents(resultChan chan *spendResult,
	blockSpent, mempoolSpent <-chan *chainntnfs.SpendDetail) {

	op := h.HtlcPoint()

	// Create a result chan to hold the results.
	result := &spendResult{}

	// hasMempoolSpend is a flag that indicates whether we have found a
	// preimage spend from the mempool. This is used to determine whether
	// to checkpoint the resolver or not when later we found the
	// corresponding block spend.
	hasMempoolSpent := false

	// Wait for a spend event to arrive.
	for {
		select {
		// If a spend event is received from the block, this outgoing
		// htlc is spent either by the remote via the preimage or by us
		// via the timeout. We can exit the loop and `claimCleanUp`
		// will feed the preimage to the beacon if found. This treats
		// the block as the final judge and the preimage spent won't
		// appear in the mempool afterwards.
		//
		// NOTE: if a reorg happens, the preimage spend can appear in
		// the mempool again. Though a rare case, we should handle it
		// in a dedicated reorg system.
		case spendDetail, ok := <-blockSpent:
			if !ok {
				result.err = fmt.Errorf("block spent err: %w",
					errResolverShuttingDown)
			} else {
				log.Debugf("Found confirmed spend of HTLC "+
					"output %s in tx=%s", op,
					spendDetail.SpenderTxHash)

				result.spend = spendDetail

				// Once confirmed, persist the state on disk if
				// we haven't seen the output's spending tx in
				// mempool before.
				//
				// NOTE: we don't checkpoint the resolver if
				// it's spending tx has already been found in
				// mempool - the resolver will take care of the
				// checkpoint in its `claimCleanUp`. If we do
				// checkpoint here, however, we'd create a new
				// record in db for the same htlc resolver
				// which won't be cleaned up later, resulting
				// the channel to stay in unresolved state.
				//
				// TODO(yy): when fee bumper is implemented, we
				// need to further check whether this is a
				// preimage spend. Also need to refactor here
				// to save us some indentation.
				if !hasMempoolSpent {
					result.err = h.checkPointSecondLevelTx()
				}
			}

			// Send the result and exit the loop.
			resultChan <- result

			return

		// If a spend event is received from the mempool, this can be
		// either the 2nd stage timeout tx or a preimage spend from the
		// remote. We will further check whether the spend reveals the
		// preimage and add it to the preimage beacon to settle the
		// incoming link.
		//
		// NOTE: we won't exit the loop here so we can continue to
		// watch for the block spend to check point the resolution.
		case spendDetail, ok := <-mempoolSpent:
			if !ok {
				result.err = fmt.Errorf("mempool spent err: %w",
					errResolverShuttingDown)

				// This is an internal error so we exit.
				resultChan <- result

				return
			}

			log.Debugf("Found mempool spend of HTLC output %s "+
				"in tx=%s", op, spendDetail.SpenderTxHash)

			// Check whether the spend reveals the preimage, if not
			// continue the loop.
			hasPreimage := isPreimageSpend(
				spendDetail,
				h.htlcResolution.SignedTimeoutTx != nil,
			)
			if !hasPreimage {
				log.Debugf("HTLC output %s spent doesn't "+
					"reveal preimage", op)
				continue
			}

			// Found the preimage spend, send the result and
			// continue the loop.
			result.spend = spendDetail
			resultChan <- result

			// Set the hasMempoolSpent flag to true so we won't
			// checkpoint the resolver again in db.
			hasMempoolSpent = true

			continue

		// If the resolver exits, we exit the goroutine.
		case <-h.quit:
			result.err = errResolverShuttingDown
			resultChan <- result

			return
		}
	}
}
