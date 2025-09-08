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
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnutils"
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

	// incomingHTLCExpiryHeight is the absolute block height at which the
	// incoming HTLC will expire. This is used as the deadline height as
	// the outgoing HTLC must be swept before its incoming HTLC expires.
	incomingHTLCExpiryHeight fn.Option[int32]
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
	h.initLogger(fmt.Sprintf("%T(%v)", h, h.outpoint()))

	return h
}

// isTaproot returns true if the htlc output is a taproot output.
func (h *htlcTimeoutResolver) isTaproot() bool {
	return txscript.IsPayToTaproot(
		h.htlcResolution.SweepSignDesc.Output.PkScript,
	)
}

// outpoint returns the outpoint of the HTLC output we're attempting to sweep.
func (h *htlcTimeoutResolver) outpoint() wire.OutPoint {
	// The primary key for this resolver will be the outpoint of the HTLC
	// on the commitment transaction itself. If this is our commitment,
	// then the output can be found within the signed timeout tx,
	// otherwise, it's just the ClaimOutpoint.
	if h.htlcResolution.SignedTimeoutTx != nil {
		return h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	}

	return h.htlcResolution.ClaimOutpoint
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) ResolverKey() []byte {
	key := newResolverID(h.outpoint())
	return key[:]
}

const (
	// expectedRemoteWitnessSuccessSize is the expected size of the witness
	// on the remote commitment transaction for an outgoing HTLC that is
	// swept on-chain by them with pre-image.
	expectedRemoteWitnessSuccessSize = 5

	// expectedLocalWitnessSuccessSize is the expected size of the witness
	// on the local commitment transaction for an outgoing HTLC that is
	// swept on-chain by them with pre-image.
	expectedLocalWitnessSuccessSize = 3

	// remotePreimageIndex index within the witness on the remote
	// commitment transaction that will hold they pre-image if they go to
	// sweep it on chain.
	remotePreimageIndex = 3

	// localPreimageIndex is the index within the witness on the local
	// commitment transaction for an outgoing HTLC that will hold the
	// pre-image if the remote party sweeps it.
	localPreimageIndex = 1

	// remoteTaprootWitnessSuccessSize is the expected size of the witness
	// on the remote commitment for taproot channels. The spend path will
	// look like
	//   - <sender sig> <receiver sig> <preimage> <success_script>
	//     <control_block>
	remoteTaprootWitnessSuccessSize = 5

	// localTaprootWitnessSuccessSize is the expected size of the witness
	// on the local commitment for taproot channels. The spend path will
	// look like
	//  - <receiver sig> <preimage> <success_script> <control_block>
	localTaprootWitnessSuccessSize = 4

	// taprootRemotePreimageIndex is the index within the witness on the
	// taproot remote commitment spend that'll hold the pre-image if the
	// remote party sweeps it.
	taprootRemotePreimageIndex = 2
)

// claimCleanUp is a helper method that's called once the HTLC output is spent
// by the remote party. It'll extract the preimage, add it to the global cache,
// and finally send the appropriate clean up message.
func (h *htlcTimeoutResolver) claimCleanUp(
	commitSpend *chainntnfs.SpendDetail) error {

	// Depending on if this is our commitment or not, then we'll be looking
	// for a different witness pattern.
	spenderIndex := commitSpend.SpenderInputIndex
	spendingInput := commitSpend.SpendingTx.TxIn[spenderIndex]

	log.Infof("%T(%v): extracting preimage! remote party spent "+
		"HTLC with tx=%v", h, h.htlcResolution.ClaimOutpoint,
		lnutils.SpewLogClosure(commitSpend.SpendingTx))

	// If this is the remote party's commitment, then we'll be looking for
	// them to spend using the second-level success transaction.
	var preimageBytes []byte
	switch {
	// For taproot channels, if the remote party has swept the HTLC, then
	// the witness stack will look like:
	//
	//   - <sender sig> <receiver sig> <preimage> <success_script>
	//     <control_block>
	case h.isTaproot() && h.htlcResolution.SignedTimeoutTx == nil:
		//nolint:ll
		preimageBytes = spendingInput.Witness[taprootRemotePreimageIndex]

	// The witness stack when the remote party sweeps the output on a
	// regular channel to them looks like:
	//
	//  - <0> <sender sig> <recvr sig> <preimage> <witness script>
	case !h.isTaproot() && h.htlcResolution.SignedTimeoutTx == nil:
		preimageBytes = spendingInput.Witness[remotePreimageIndex]

	// If this is a taproot channel, and there's only a single witness
	// element, then we're actually on the losing side of a breach
	// attempt...
	case h.isTaproot() && len(spendingInput.Witness) == 1:
		return fmt.Errorf("breach attempt failed")

	// Otherwise, they'll be spending directly from our commitment output.
	// In which case the witness stack looks like:
	//
	//  - <sig> <preimage> <witness script>
	//
	// For taproot channels, this looks like:
	//  - <receiver sig> <preimage> <success_script> <control_block>
	//
	// So we can target the same index.
	default:
		preimageBytes = spendingInput.Witness[localPreimageIndex]
	}

	preimage, err := lntypes.MakePreimage(preimageBytes)
	if err != nil {
		return fmt.Errorf("unable to create pre-image from witness: %w",
			err)
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
		return err
	}
	h.markResolved()

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

	return h.Checkpoint(h, report)
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
	//
	//nolint:ll
	outPointToWatch := h.htlcResolution.SignedTimeoutTx.TxIn[0].PreviousOutPoint
	witness := h.htlcResolution.SignedTimeoutTx.TxIn[0].Witness

	var (
		scriptToWatch []byte
		err           error
	)
	switch {
	// For taproot channels, then final witness element is the control
	// block, and the one before it the witness script. We can use both of
	// these together to reconstruct the taproot output key, then map that
	// into a v1 witness program.
	case h.isTaproot():
		// First, we'll parse the control block into something we can
		// use.
		ctrlBlockBytes := witness[len(witness)-1]
		ctrlBlock, err := txscript.ParseControlBlock(ctrlBlockBytes)
		if err != nil {
			return nil, nil, err
		}

		// With the control block, we'll grab the witness script, then
		// use that to derive the tapscript root.
		witnessScript := witness[len(witness)-2]
		tapscriptRoot := ctrlBlock.RootHash(witnessScript)

		// Once we have the root, then we can derive the output key
		// from the internal key, then turn that into a witness
		// program.
		outputKey := txscript.ComputeTaprootOutputKey(
			ctrlBlock.InternalKey, tapscriptRoot,
		)
		scriptToWatch, err = txscript.PayToTaprootScript(outputKey)
		if err != nil {
			return nil, nil, err
		}

	// For regular channels, the witness script is the last element on the
	// stack. We can then use this to re-derive the output that we're
	// watching on chain.
	default:
		scriptToWatch, err = input.WitnessScriptHash(
			witness[len(witness)-1],
		)
	}
	if err != nil {
		return nil, nil, err
	}

	return &outPointToWatch, scriptToWatch, nil
}

// isPreimageSpend returns true if the passed spend on the specified commitment
// is a success spend that reveals the pre-image or not.
func isPreimageSpend(isTaproot bool, spend *chainntnfs.SpendDetail,
	localCommit bool) bool {

	// Based on the spending input index and transaction, obtain the
	// witness that tells us what type of spend this is.
	spenderIndex := spend.SpenderInputIndex
	spendingInput := spend.SpendingTx.TxIn[spenderIndex]
	spendingWitness := spendingInput.Witness

	switch {
	// If this is a taproot remote commitment, then we can detect the type
	// of spend via the leaf revealed in the control block and the witness
	// itself.
	//
	// The keyspend (revocation path) is just a single signature, while the
	// timeout and success paths are most distinct.
	//
	// The success path will look like:
	//
	//   - <sender sig> <receiver sig> <preimage> <success_script>
	//     <control_block>
	case isTaproot && !localCommit:
		return checkSizeAndIndex(
			spendingWitness, remoteTaprootWitnessSuccessSize,
			taprootRemotePreimageIndex,
		)

	// Otherwise, then if this is our local commitment transaction, then if
	// they're sweeping the transaction, it'll be directly from the output,
	// skipping the second level.
	//
	// In this case, then there're two main tapscript paths, with the
	// success case look like:
	//
	//  - <receiver sig> <preimage> <success_script> <control_block>
	case isTaproot && localCommit:
		return checkSizeAndIndex(
			spendingWitness, localTaprootWitnessSuccessSize,
			localPreimageIndex,
		)

	// If this is the non-taproot, remote commitment then the only possible
	// spends for outgoing HTLCs are:
	//
	//  RECVR: <0> <sender sig> <recvr sig> <preimage> (2nd level success spend)
	//  REVOK: <sig> <key>
	//  SENDR: <sig> 0
	//
	// In this case, if 5 witness elements are present (factoring the
	// witness script), and the 3rd element is the size of the pre-image,
	// then this is a remote spend. If not, then we swept it ourselves, or
	// revoked their output.
	case !isTaproot && !localCommit:
		return checkSizeAndIndex(
			spendingWitness, expectedRemoteWitnessSuccessSize,
			remotePreimageIndex,
		)

	// Otherwise, for our non-taproot commitment, the only possible spends
	// for an outgoing HTLC are:
	//
	//  SENDR: <0> <sendr sig>  <recvr sig> <0> (2nd level timeout)
	//  RECVR: <recvr sig>  <preimage>
	//  REVOK: <revoke sig> <revoke key>
	//
	// So the only success case has the pre-image as the 2nd (index 1)
	// element in the witness.
	case !isTaproot:
		fallthrough

	default:
		return checkSizeAndIndex(
			spendingWitness, expectedLocalWitnessSuccessSize,
			localPreimageIndex,
		)
	}
}

// checkSizeAndIndex checks that the witness is of the expected size and that
// the witness element at the specified index is of the expected size.
func checkSizeAndIndex(witness wire.TxWitness, size, index int) bool {
	if len(witness) != size {
		return false
	}

	return len(witness[index]) == lntypes.HashSize
}

// Resolve kicks off full resolution of an outgoing HTLC output. If it's our
// commitment, it isn't resolved until we see the second level HTLC txn
// confirmed. If it's the remote party's commitment, we don't resolve until we
// see a direct sweep via the timeout clause.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if h.IsResolved() {
		h.log.Errorf("already resolved")
		return nil, nil
	}

	// If this is an output on the remote party's commitment transaction,
	// use the direct-spend path to sweep the htlc.
	if h.isRemoteCommitOutput() {
		return nil, h.resolveRemoteCommitOutput()
	}

	// If this is a zero-fee HTLC, we now handle the spend from our
	// commitment transaction.
	if h.isZeroFeeOutput() {
		return nil, h.resolveTimeoutTx()
	}

	// If this is an output on our own commitment using pre-anchor channel
	// type, we will let the utxo nursery handle it.
	return nil, h.resolveSecondLevelTxLegacy()
}

// sweepTimeoutTx sends a second level timeout transaction to the sweeper.
// This transaction uses the SINGLE|ANYONECANPAY flag.
func (h *htlcTimeoutResolver) sweepTimeoutTx() error {
	var inp input.Input
	if h.isTaproot() {
		inp = lnutils.Ptr(input.MakeHtlcSecondLevelTimeoutTaprootInput(
			h.htlcResolution.SignedTimeoutTx,
			h.htlcResolution.SignDetails,
			h.broadcastHeight,
			input.WithResolutionBlob(
				h.htlcResolution.ResolutionBlob,
			),
		))
	} else {
		inp = lnutils.Ptr(input.MakeHtlcSecondLevelTimeoutAnchorInput(
			h.htlcResolution.SignedTimeoutTx,
			h.htlcResolution.SignDetails,
			h.broadcastHeight,
		))
	}

	// Calculate the budget.
	budget := calculateBudget(
		btcutil.Amount(inp.SignDesc().Output.Value),
		h.Budget.DeadlineHTLCRatio, h.Budget.DeadlineHTLC,
	)

	h.log.Infof("offering 2nd-level HTLC timeout tx to sweeper "+
		"with deadline=%v, budget=%v", h.incomingHTLCExpiryHeight,
		budget)

	// For an outgoing HTLC, it must be swept before the RefundTimeout of
	// its incoming HTLC is reached.
	_, err := h.Sweeper.SweepInput(
		inp,
		sweep.Params{
			Budget:         budget,
			DeadlineHeight: h.incomingHTLCExpiryHeight,
		},
	)
	if err != nil {
		return err
	}

	return nil
}

// resolveSecondLevelTxLegacy sends a second level timeout transaction to the
// utxo nursery. This transaction uses the legacy SIGHASH_ALL flag.
func (h *htlcTimeoutResolver) resolveSecondLevelTxLegacy() error {
	h.log.Debug("incubating htlc output")

	// The utxo nursery will take care of broadcasting the second-level
	// timeout tx and sweeping its output once it confirms.
	err := h.IncubateOutputs(
		h.ChanPoint, fn.Some(h.htlcResolution),
		fn.None[lnwallet.IncomingHtlcResolution](),
		h.broadcastHeight, h.incomingHTLCExpiryHeight,
	)
	if err != nil {
		return err
	}

	return h.resolveTimeoutTx()
}

// sweepDirectHtlcOutput sends the direct spend of the HTLC output to the
// sweeper. This is used when the remote party goes on chain, and we're able to
// sweep an HTLC we offered after a timeout. Only the CLTV encumbered outputs
// are resolved via this path.
func (h *htlcTimeoutResolver) sweepDirectHtlcOutput() error {
	var htlcWitnessType input.StandardWitnessType
	if h.isTaproot() {
		htlcWitnessType = input.TaprootHtlcOfferedRemoteTimeout
	} else {
		htlcWitnessType = input.HtlcOfferedRemoteTimeout
	}

	sweepInput := input.NewCsvInputWithCltv(
		&h.htlcResolution.ClaimOutpoint, htlcWitnessType,
		&h.htlcResolution.SweepSignDesc, h.broadcastHeight,
		h.htlcResolution.CsvDelay, h.htlcResolution.Expiry,
		input.WithResolutionBlob(h.htlcResolution.ResolutionBlob),
	)

	// Calculate the budget.
	budget := calculateBudget(
		btcutil.Amount(sweepInput.SignDesc().Output.Value),
		h.Budget.DeadlineHTLCRatio, h.Budget.DeadlineHTLC,
	)

	log.Infof("%T(%x): offering offered remote timeout HTLC output to "+
		"sweeper with deadline %v and budget=%v at height=%v",
		h, h.htlc.RHash[:], h.incomingHTLCExpiryHeight, budget,
		h.broadcastHeight)

	_, err := h.Sweeper.SweepInput(
		sweepInput,
		sweep.Params{
			Budget: budget,

			// This is an outgoing HTLC, so we want to make sure
			// that we sweep it before the incoming HTLC expires.
			DeadlineHeight: h.incomingHTLCExpiryHeight,
		},
	)
	if err != nil {
		return err
	}

	return nil
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

	// We'll block here until either we exit, or the HTLC output on the
	// commitment transaction has been spent.
	spend, err := waitForSpend(
		op, pkScript, h.broadcastHeight, h.Notifier, h.quit,
	)
	if err != nil {
		return nil, err
	}

	return spend, nil
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (h *htlcTimeoutResolver) Stop() {
	h.log.Debugf("stopping...")
	defer h.log.Debugf("stopped")

	close(h.quit)
}

// report returns a report on the resolution state of the contract.
func (h *htlcTimeoutResolver) report() *ContractReport {
	// If we have a SignedTimeoutTx but no SignDetails, this is a local
	// commitment for a non-anchor channel, which was handled by the utxo
	// nursery.
	if h.htlcResolution.SignDetails == nil && h.
		htlcResolution.SignedTimeoutTx != nil {
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

	// If there's no timeout transaction, then we're already effectively in
	// level two.
	stage := uint32(1)
	if h.htlcResolution.SignedTimeoutTx == nil {
		stage = 2
	}

	h.currentReport = ContractReport{
		Outpoint:       h.htlcResolution.ClaimOutpoint,
		Type:           ReportOutputOutgoingHtlc,
		Amount:         finalAmt,
		MaturityHeight: h.htlcResolution.Expiry,
		LimboBalance:   finalAmt,
		Stage:          stage,
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
	if err := binary.Write(w, endian, h.IsResolved()); err != nil {
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
	h.initLogger(fmt.Sprintf("%T(%v)", h, h.outpoint()))

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

// SupplementDeadline sets the incomingHTLCExpiryHeight for this outgoing htlc
// resolver.
//
// NOTE: Part of the htlcContractResolver interface.
func (h *htlcTimeoutResolver) SupplementDeadline(d fn.Option[int32]) {
	h.incomingHTLCExpiryHeight = d
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
				h.isTaproot(), spendDetail,
				!h.isRemoteCommitOutput(),
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

			continue

		// If the resolver exits, we exit the goroutine.
		case <-h.quit:
			result.err = errResolverShuttingDown
			resultChan <- result

			return
		}
	}
}

// isRemoteCommitOutput returns a bool to indicate whether the htlc output is
// on the remote commitment.
func (h *htlcTimeoutResolver) isRemoteCommitOutput() bool {
	// If we don't have a timeout transaction, then this means that this is
	// an output on the remote party's commitment transaction.
	return h.htlcResolution.SignedTimeoutTx == nil
}

// isZeroFeeOutput returns a boolean indicating whether the htlc output is from
// a anchor-enabled channel, which uses the sighash SINGLE|ANYONECANPAY.
func (h *htlcTimeoutResolver) isZeroFeeOutput() bool {
	// If we have non-nil SignDetails, this means it has a 2nd level HTLC
	// transaction that is signed using sighash SINGLE|ANYONECANPAY (the
	// case for anchor type channels). In this case we can re-sign it and
	// attach fees at will.
	return h.htlcResolution.SignedTimeoutTx != nil &&
		h.htlcResolution.SignDetails != nil
}

// waitHtlcSpendAndCheckPreimage waits for the htlc output to be spent and
// checks whether the spending reveals the preimage. If the preimage is found,
// it will be added to the preimage beacon to settle the incoming link, and a
// nil spend details will be returned. Otherwise, the spend details will be
// returned, indicating this is a non-preimage spend.
func (h *htlcTimeoutResolver) waitHtlcSpendAndCheckPreimage() (
	*chainntnfs.SpendDetail, error) {

	// Wait for the htlc output to be spent, which can happen in one of the
	// paths,
	// 1. The remote party spends the htlc output using the preimage.
	// 2. The local party spends the htlc timeout tx from the local
	//    commitment.
	// 3. The local party spends the htlc output directlt from the remote
	//    commitment.
	spend, err := h.watchHtlcSpend()
	if err != nil {
		return nil, err
	}

	// If the spend reveals the pre-image, then we'll enter the clean up
	// workflow to pass the preimage back to the incoming link, add it to
	// the witness cache, and exit.
	if isPreimageSpend(h.isTaproot(), spend, !h.isRemoteCommitOutput()) {
		return nil, h.claimCleanUp(spend)
	}

	return spend, nil
}

// sweepTimeoutTxOutput attempts to sweep the output of the second level
// timeout tx.
func (h *htlcTimeoutResolver) sweepTimeoutTxOutput() error {
	h.log.Debugf("sweeping output %v from 2nd-level HTLC timeout tx",
		h.htlcResolution.ClaimOutpoint)

	// This should be non-blocking as we will only attempt to sweep the
	// output when the second level tx has already been confirmed. In other
	// words, waitHtlcSpendAndCheckPreimage will return immediately.
	commitSpend, err := h.waitHtlcSpendAndCheckPreimage()
	if err != nil {
		return err
	}

	// Exit early if the spend is nil, as this means it's a remote spend
	// using the preimage path, which is handled in claimCleanUp.
	if commitSpend == nil {
		h.log.Infof("preimage spend detected, skipping 2nd-level " +
			"HTLC output sweep")

		return nil
	}

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
		h.log.Infof("waiting for CSV and CLTV lock to expire at "+
			"height %v", waitHeight)
	} else {
		h.log.Infof("waiting for CSV lock to expire at height %v",
			waitHeight)
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

	var witType input.StandardWitnessType
	if h.isTaproot() {
		witType = input.TaprootHtlcOfferedTimeoutSecondLevel
	} else {
		witType = input.HtlcOfferedTimeoutSecondLevel
	}

	// Let the sweeper sweep the second-level output now that the CSV/CLTV
	// locks have expired.
	inp := h.makeSweepInput(
		op, witType,
		input.LeaseHtlcOfferedTimeoutSecondLevel,
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

	h.log.Infof("offering output from 2nd-level timeout tx to sweeper "+
		"with no deadline and budget=%v", budget)

	// TODO(yy): use the result chan returned from SweepInput to get the
	// confirmation status of this sweeping tx so we don't need to make
	// another subscription via `RegisterSpendNtfn` for this outpoint here
	// in the resolver.
	_, err = h.Sweeper.SweepInput(
		inp,
		sweep.Params{
			Budget: budget,

			// For second level success tx, there's no rush
			// to get it confirmed, so we use a nil
			// deadline.
			DeadlineHeight: fn.None[int32](),
		},
	)

	return err
}

// checkpointStageOne creates a checkpoint for the first stage of the htlc
// timeout transaction. This is used to ensure that the resolver can resume
// watching for the second stage spend in case of a restart.
func (h *htlcTimeoutResolver) checkpointStageOne(
	spendTxid chainhash.Hash) error {

	h.log.Debugf("checkpoint stage one spend of HTLC output %v, spent "+
		"in tx %v", h.outpoint(), spendTxid)

	// Now that the second-level transaction has confirmed, we checkpoint
	// the state so we'll go to the next stage in case of restarts.
	h.outputIncubating = true

	// Create stage-one report.
	report := &channeldb.ResolverReport{
		OutPoint:        h.outpoint(),
		Amount:          h.htlc.Amt.ToSatoshis(),
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeFirstStage,
		SpendTxID:       &spendTxid,
	}

	// At this point, the second-level transaction is sufficiently
	// confirmed. We can now send back our clean up message, failing the
	// HTLC on the incoming link.
	failureMsg := &lnwire.FailPermanentChannelFailure{}
	err := h.DeliverResolutionMsg(ResolutionMsg{
		SourceChan: h.ShortChanID,
		HtlcIndex:  h.htlc.HtlcIndex,
		Failure:    failureMsg,
	})
	if err != nil {
		return err
	}

	return h.Checkpoint(h, report)
}

// checkpointClaim checkpoints the timeout resolver with the reports it needs.
func (h *htlcTimeoutResolver) checkpointClaim(
	spendDetail *chainntnfs.SpendDetail) error {

	h.log.Infof("resolving htlc with incoming fail msg, output=%v "+
		"confirmed in tx=%v", spendDetail.SpentOutPoint,
		spendDetail.SpenderTxHash)

	// Create a resolver report for the claiming of the HTLC.
	amt := btcutil.Amount(h.htlcResolution.SweepSignDesc.Output.Value)
	report := &channeldb.ResolverReport{
		OutPoint:        *spendDetail.SpentOutPoint,
		Amount:          amt,
		ResolverType:    channeldb.ResolverTypeOutgoingHtlc,
		ResolverOutcome: channeldb.ResolverOutcomeTimeout,
		SpendTxID:       spendDetail.SpenderTxHash,
	}

	// Finally, we checkpoint the resolver with our report(s).
	h.markResolved()

	return h.Checkpoint(h, report)
}

// resolveRemoteCommitOutput handles sweeping an HTLC output on the remote
// commitment with via the timeout path. In this case we can sweep the output
// directly, and don't have to broadcast a second-level transaction.
func (h *htlcTimeoutResolver) resolveRemoteCommitOutput() error {
	h.log.Debug("waiting for direct-timeout spend of the htlc to confirm")

	// Wait for the direct-timeout HTLC sweep tx to confirm.
	spend, err := h.watchHtlcSpend()
	if err != nil {
		return err
	}

	// If the spend reveals the preimage, then we'll enter the clean up
	// workflow to pass the preimage back to the incoming link, add it to
	// the witness cache, and exit.
	if isPreimageSpend(h.isTaproot(), spend, !h.isRemoteCommitOutput()) {
		return h.claimCleanUp(spend)
	}

	// Send the clean up msg to fail the incoming HTLC.
	failureMsg := &lnwire.FailPermanentChannelFailure{}
	err = h.DeliverResolutionMsg(ResolutionMsg{
		SourceChan: h.ShortChanID,
		HtlcIndex:  h.htlc.HtlcIndex,
		Failure:    failureMsg,
	})
	if err != nil {
		return err
	}

	// TODO(yy): should also update the `RecoveredBalance` and
	// `LimboBalance` like other paths?

	// Checkpoint the resolver, and write the outcome to disk.
	return h.checkpointClaim(spend)
}

// resolveTimeoutTx waits for the sweeping tx of the second-level
// timeout tx to confirm and offers the output from the timeout tx to the
// sweeper.
func (h *htlcTimeoutResolver) resolveTimeoutTx() error {
	h.log.Debug("waiting for first-stage 2nd-level HTLC timeout tx to " +
		"confirm")

	// Wait for the second level transaction to confirm.
	spend, err := h.watchHtlcSpend()
	if err != nil {
		return err
	}

	// If the spend reveals the preimage, then we'll enter the clean up
	// workflow to pass the preimage back to the incoming link, add it to
	// the witness cache, and exit.
	if isPreimageSpend(h.isTaproot(), spend, !h.isRemoteCommitOutput()) {
		return h.claimCleanUp(spend)
	}

	op := h.htlcResolution.ClaimOutpoint
	spenderTxid := *spend.SpenderTxHash

	// If the timeout tx is a re-signed tx, we will need to find the actual
	// spent outpoint from the spending tx.
	if h.isZeroFeeOutput() {
		op = wire.OutPoint{
			Hash:  spenderTxid,
			Index: spend.SpenderInputIndex,
		}
	}

	// If the 2nd-stage sweeping has already been started, we can
	// fast-forward to start the resolving process for the stage two
	// output.
	if h.outputIncubating {
		return h.resolveTimeoutTxOutput(op)
	}

	h.log.Infof("2nd-level HTLC timeout tx=%v confirmed", spenderTxid)

	// Start the process to sweep the output from the timeout tx.
	if h.isZeroFeeOutput() {
		err = h.sweepTimeoutTxOutput()
		if err != nil {
			return err
		}
	}

	// Create a checkpoint since the timeout tx is confirmed and the sweep
	// request has been made.
	if err := h.checkpointStageOne(spenderTxid); err != nil {
		return err
	}

	// Start the resolving process for the stage two output.
	return h.resolveTimeoutTxOutput(op)
}

// resolveTimeoutTxOutput waits for the spend of the output from the 2nd-level
// timeout tx.
func (h *htlcTimeoutResolver) resolveTimeoutTxOutput(op wire.OutPoint) error {
	h.log.Debugf("waiting for second-stage 2nd-level timeout tx output %v "+
		"to be spent after csv_delay=%v", op, h.htlcResolution.CsvDelay)

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

	return h.checkpointClaim(spend)
}

// Launch creates an input based on the details of the outgoing htlc resolution
// and offers it to the sweeper.
func (h *htlcTimeoutResolver) Launch() error {
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
	// use the direct timeout spend path.
	//
	// NOTE: When the outputIncubating is false, it means that the output
	// has been offered to the utxo nursery as starting in 0.18.4, we
	// stopped marking this flag for direct timeout spends (#9062). In that
	// case, we will do nothing and let the utxo nursery handle it.
	case h.isRemoteCommitOutput() && !h.outputIncubating:
		return h.sweepDirectHtlcOutput()

	// If this is an anchor type channel, we now sweep either the
	// second-level timeout tx or the output from the second-level timeout
	// tx.
	case h.isZeroFeeOutput():
		// If the second-level timeout tx has already been swept, we
		// can go ahead and sweep its output.
		if h.outputIncubating {
			return h.sweepTimeoutTxOutput()
		}

		// Otherwise, sweep the second level tx.
		return h.sweepTimeoutTx()

	// If this is an output on our own commitment using pre-anchor channel
	// type, we will let the utxo nursery handle it via Resolve.
	//
	// TODO(yy): handle the legacy output by offering it to the sweeper.
	default:
		return nil
	}
}
