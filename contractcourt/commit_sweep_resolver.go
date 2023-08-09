package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

const (
	// commitOutputConfTarget is the default confirmation target we'll use
	// for sweeps of commit outputs that belong to us.
	commitOutputConfTarget = 6
)

// commitSweepResolver is a resolver that will attempt to sweep the commitment
// output paying to us, in the case that the remote party broadcasts their
// version of the commitment transaction. We can sweep this output immediately,
// as it doesn't have a time-lock delay.
type commitSweepResolver struct {
	// commitResolution contains all data required to successfully sweep
	// this HTLC on-chain.
	commitResolution lnwallet.CommitOutputResolution

	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// chanPoint is the channel point of the original contract.
	chanPoint wire.OutPoint

	// channelInitiator denotes whether the party responsible for resolving
	// the contract initiated the channel.
	channelInitiator bool

	// leaseExpiry denotes the additional waiting period the contract must
	// hold until it can be resolved. This waiting period is known as the
	// expiration of a script-enforced leased channel and only applies to
	// the channel initiator.
	//
	// NOTE: This value should only be set when the contract belongs to a
	// leased channel.
	leaseExpiry uint32

	// chanType denotes the type of channel the contract belongs to.
	chanType channeldb.ChannelType

	// currentReport stores the current state of the resolver for reporting
	// over the rpc interface.
	currentReport ContractReport

	// reportLock prevents concurrent access to the resolver report.
	reportLock sync.Mutex

	contractResolverKit
}

// newCommitSweepResolver instantiates a new direct commit output resolver.
func newCommitSweepResolver(res lnwallet.CommitOutputResolution,
	broadcastHeight uint32, chanPoint wire.OutPoint,
	resCfg ResolverConfig) *commitSweepResolver {

	r := &commitSweepResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		commitResolution:    res,
		broadcastHeight:     broadcastHeight,
		chanPoint:           chanPoint,
	}

	r.initLogger(r)
	r.initReport()

	return r
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
func (c *commitSweepResolver) ResolverKey() []byte {
	key := newResolverID(c.commitResolution.SelfOutPoint)
	return key[:]
}

// waitForHeight registers for block notifications and waits for the provided
// block height to be reached.
func waitForHeight(waitHeight uint32, notifier chainntnfs.ChainNotifier,
	quit <-chan struct{}) error {

	// Register for block epochs. After registration, the current height
	// will be sent on the channel immediately.
	blockEpochs, err := notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return err
	}
	defer blockEpochs.Cancel()

	for {
		select {
		case newBlock, ok := <-blockEpochs.Epochs:
			if !ok {
				return errResolverShuttingDown
			}
			height := newBlock.Height
			if height >= int32(waitHeight) {
				return nil
			}

		case <-quit:
			return errResolverShuttingDown
		}
	}
}

// waitForSpend waits for the given outpoint to be spent, and returns the
// details of the spending tx.
func waitForSpend(op *wire.OutPoint, pkScript []byte, heightHint uint32,
	notifier chainntnfs.ChainNotifier, quit <-chan struct{}) (
	*chainntnfs.SpendDetail, error) {

	spendNtfn, err := notifier.RegisterSpendNtfn(
		op, pkScript, heightHint,
	)
	if err != nil {
		return nil, err
	}

	select {
	case spendDetail, ok := <-spendNtfn.Spend:
		if !ok {
			return nil, errResolverShuttingDown
		}

		return spendDetail, nil

	case <-quit:
		return nil, errResolverShuttingDown
	}
}

// getCommitTxConfHeight waits for confirmation of the commitment tx and
// returns the confirmation height.
func (c *commitSweepResolver) getCommitTxConfHeight() (uint32, error) {
	txID := c.commitResolution.SelfOutPoint.Hash
	signDesc := c.commitResolution.SelfOutputSignDesc
	pkScript := signDesc.Output.PkScript

	const confDepth = 1

	confChan, err := c.Notifier.RegisterConfirmationsNtfn(
		&txID, pkScript, confDepth, c.broadcastHeight,
	)
	if err != nil {
		return 0, err
	}
	defer confChan.Cancel()

	select {
	case txConfirmation, ok := <-confChan.Confirmed:
		if !ok {
			return 0, fmt.Errorf("cannot get confirmation "+
				"for commit tx %v", txID)
		}

		return txConfirmation.BlockHeight, nil

	case <-c.quit:
		return 0, errResolverShuttingDown
	}
}

// Resolve instructs the contract resolver to resolve the output on-chain. Once
// the output has been *fully* resolved, the function should return immediately
// with a nil ContractResolver value for the first return value.  In the case
// that the contract requires further resolution, then another resolve is
// returned.
//
// NOTE: This function MUST be run as a goroutine.
func (c *commitSweepResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if c.resolved {
		return nil, nil
	}

	confHeight, err := c.getCommitTxConfHeight()
	if err != nil {
		return nil, err
	}

	// Wait up until the CSV expires, unless we also have a CLTV that
	// expires after.
	unlockHeight := confHeight + c.commitResolution.MaturityDelay
	if c.hasCLTV() {
		unlockHeight = uint32(math.Max(
			float64(unlockHeight), float64(c.leaseExpiry),
		))
	}

	c.log.Debugf("commit conf_height=%v, unlock_height=%v",
		confHeight, unlockHeight)

	// Update report now that we learned the confirmation height.
	c.reportLock.Lock()
	c.currentReport.MaturityHeight = unlockHeight
	c.reportLock.Unlock()

	// If there is a csv/cltv lock, we'll wait for that.
	if c.commitResolution.MaturityDelay > 0 || c.hasCLTV() {
		// Determine what height we should wait until for the locks to
		// expire.
		var waitHeight uint32
		switch {
		// If we have both a csv and cltv lock, we'll need to look at
		// both and see which expires later.
		case c.commitResolution.MaturityDelay > 0 && c.hasCLTV():
			c.log.Debugf("waiting for CSV and CLTV lock to expire "+
				"at height %v", unlockHeight)
			// If the CSV expires after the CLTV, or there is no
			// CLTV, then we can broadcast a sweep a block before.
			// Otherwise, we need to broadcast at our expected
			// unlock height.
			waitHeight = uint32(math.Max(
				float64(unlockHeight-1), float64(c.leaseExpiry),
			))

		// If we only have a csv lock, wait for the height before the
		// lock expires as the spend path should be unlocked by then.
		case c.commitResolution.MaturityDelay > 0:
			c.log.Debugf("waiting for CSV lock to expire at "+
				"height %v", unlockHeight)
			waitHeight = unlockHeight - 1
		}

		err := waitForHeight(waitHeight, c.Notifier, c.quit)
		if err != nil {
			return nil, err
		}
	}

	var (
		isLocalCommitTx bool

		signDesc = c.commitResolution.SelfOutputSignDesc
	)
	switch {
	// For taproot channels, we'll know if this is the local commit based
	// on the witness script. For local channels, the witness script has an
	// OP_DROP value.
	//
	// TODO(roasbeef): revisit this after the script changes
	//  * otherwise need to base off the key in script or the CSV value
	//  (script num encode)
	case c.chanType.IsTaproot():
		scriptLen := len(signDesc.WitnessScript)
		isLocalCommitTx = signDesc.WitnessScript[scriptLen-1] ==
			txscript.OP_DROP

	// The output is on our local commitment if the script starts with
	// OP_IF for the revocation clause. On the remote commitment it will
	// either be a regular P2WKH or a simple sig spend with a CSV delay.
	default:
		isLocalCommitTx = signDesc.WitnessScript[0] == txscript.OP_IF
	}
	isDelayedOutput := c.commitResolution.MaturityDelay != 0

	c.log.Debugf("isDelayedOutput=%v, isLocalCommitTx=%v", isDelayedOutput,
		isLocalCommitTx)

	// There're three types of commitments, those that have tweaks for the
	// remote key (us in this case), those that don't, and a third where
	// there is no tweak and the output is delayed. On the local commitment
	// our output will always be delayed. We'll rely on the presence of the
	// commitment tweak to discern which type of commitment this is.
	var witnessType input.WitnessType
	switch {
	// The local delayed output for a taproot channel.
	case isLocalCommitTx && c.chanType.IsTaproot():
		witnessType = input.TaprootLocalCommitSpend

	// The CSV 1 delayed output for a taproot channel.
	case !isLocalCommitTx && c.chanType.IsTaproot():
		witnessType = input.TaprootRemoteCommitSpend

	// Delayed output to us on our local commitment for a channel lease in
	// which we are the initiator.
	case isLocalCommitTx && c.hasCLTV():
		witnessType = input.LeaseCommitmentTimeLock

	// Delayed output to us on our local commitment.
	case isLocalCommitTx:
		witnessType = input.CommitmentTimeLock

	// A confirmed output to us on the remote commitment for a channel lease
	// in which we are the initiator.
	case isDelayedOutput && c.hasCLTV():
		witnessType = input.LeaseCommitmentToRemoteConfirmed

	// A confirmed output to us on the remote commitment.
	case isDelayedOutput:
		witnessType = input.CommitmentToRemoteConfirmed

	// A non-delayed output on the remote commitment where the key is
	// tweakless.
	case c.commitResolution.SelfOutputSignDesc.SingleTweak == nil:
		witnessType = input.CommitSpendNoDelayTweakless

	// A non-delayed output on the remote commitment where the key is
	// tweaked.
	default:
		witnessType = input.CommitmentNoDelay
	}

	c.log.Infof("Sweeping with witness type: %v", witnessType)

	// We'll craft an input with all the information required for the
	// sweeper to create a fully valid sweeping transaction to recover
	// these coins.
	var inp *input.BaseInput
	if c.hasCLTV() {
		inp = input.NewCsvInputWithCltv(
			&c.commitResolution.SelfOutPoint, witnessType,
			&c.commitResolution.SelfOutputSignDesc,
			c.broadcastHeight, c.commitResolution.MaturityDelay,
			c.leaseExpiry,
		)
	} else {
		inp = input.NewCsvInput(
			&c.commitResolution.SelfOutPoint, witnessType,
			&c.commitResolution.SelfOutputSignDesc,
			c.broadcastHeight, c.commitResolution.MaturityDelay,
		)
	}

	// TODO(roasbeef): instead of ading ctrl block to the sign desc, make
	// new input type, have sweeper set it?

	// With our input constructed, we'll now offer it to the
	// sweeper.
	c.log.Infof("sweeping commit output")

	feePref := sweep.FeePreference{ConfTarget: commitOutputConfTarget}
	resultChan, err := c.Sweeper.SweepInput(inp, sweep.Params{Fee: feePref})
	if err != nil {
		c.log.Errorf("unable to sweep input: %v", err)

		return nil, err
	}

	var sweepTxID chainhash.Hash

	// Sweeper is going to join this input with other inputs if possible
	// and publish the sweep tx. When the sweep tx confirms, it signals us
	// through the result channel with the outcome. Wait for this to
	// happen.
	outcome := channeldb.ResolverOutcomeClaimed
	select {
	case sweepResult := <-resultChan:
		switch sweepResult.Err {
		// If the remote party was able to sweep this output it's
		// likely what we sent was actually a revoked commitment.
		// Report the error and continue to wrap up the contract.
		case sweep.ErrRemoteSpend:
			c.log.Warnf("local commitment output was swept by "+
				"remote party via %v", sweepResult.Tx.TxHash())
			outcome = channeldb.ResolverOutcomeUnclaimed

		// No errors, therefore continue processing.
		case nil:
			c.log.Infof("local commitment output fully resolved by "+
				"sweep tx: %v", sweepResult.Tx.TxHash())
		// Unknown errors.
		default:
			c.log.Errorf("unable to sweep input: %v",
				sweepResult.Err)

			return nil, sweepResult.Err
		}

		sweepTxID = sweepResult.Tx.TxHash()

	case <-c.quit:
		return nil, errResolverShuttingDown
	}

	// Funds have been swept and balance is no longer in limbo.
	c.reportLock.Lock()
	if outcome == channeldb.ResolverOutcomeClaimed {
		// We only record the balance as recovered if it actually came
		// back to us.
		c.currentReport.RecoveredBalance = c.currentReport.LimboBalance
	}
	c.currentReport.LimboBalance = 0
	c.reportLock.Unlock()
	report := c.currentReport.resolverReport(
		&sweepTxID, channeldb.ResolverTypeCommit, outcome,
	)
	c.resolved = true

	// Checkpoint the resolver with a closure that will write the outcome
	// of the resolver and its sweep transaction to disk.
	return nil, c.Checkpoint(c, report)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Stop() {
	close(c.quit)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) IsResolved() bool {
	return c.resolved
}

// SupplementState allows the user of a ContractResolver to supplement it with
// state required for the proper resolution of a contract.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) SupplementState(state *channeldb.OpenChannel) {
	if state.ChanType.HasLeaseExpiration() {
		c.leaseExpiry = state.ThawHeight
	}
	c.channelInitiator = state.IsInitiator
	c.chanType = state.ChanType
}

// hasCLTV denotes whether the resolver must wait for an additional CLTV to
// expire before resolving the contract.
func (c *commitSweepResolver) hasCLTV() bool {
	return c.channelInitiator && c.leaseExpiry > 0
}

// Encode writes an encoded version of the ContractResolver into the passed
// Writer.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Encode(w io.Writer) error {
	if err := encodeCommitResolution(w, &c.commitResolution); err != nil {
		return err
	}

	if err := binary.Write(w, endian, c.resolved); err != nil {
		return err
	}
	if err := binary.Write(w, endian, c.broadcastHeight); err != nil {
		return err
	}
	if _, err := w.Write(c.chanPoint.Hash[:]); err != nil {
		return err
	}
	err := binary.Write(w, endian, c.chanPoint.Index)
	if err != nil {
		return err
	}

	// Previously a sweep tx was serialized at this point. Refactoring
	// removed this, but keep in mind that this data may still be present in
	// the database.

	return nil
}

// newCommitSweepResolverFromReader attempts to decode an encoded
// ContractResolver from the passed Reader instance, returning an active
// ContractResolver instance.
func newCommitSweepResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*commitSweepResolver, error) {

	c := &commitSweepResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
	}

	if err := decodeCommitResolution(r, &c.commitResolution); err != nil {
		return nil, err
	}

	if err := binary.Read(r, endian, &c.resolved); err != nil {
		return nil, err
	}
	if err := binary.Read(r, endian, &c.broadcastHeight); err != nil {
		return nil, err
	}
	_, err := io.ReadFull(r, c.chanPoint.Hash[:])
	if err != nil {
		return nil, err
	}
	err = binary.Read(r, endian, &c.chanPoint.Index)
	if err != nil {
		return nil, err
	}

	// Previously a sweep tx was deserialized at this point. Refactoring
	// removed this, but keep in mind that this data may still be present in
	// the database.

	c.initLogger(c)
	c.initReport()

	return c, nil
}

// report returns a report on the resolution state of the contract.
func (c *commitSweepResolver) report() *ContractReport {
	c.reportLock.Lock()
	defer c.reportLock.Unlock()

	cpy := c.currentReport
	return &cpy
}

// initReport initializes the pending channels report for this resolver.
func (c *commitSweepResolver) initReport() {
	amt := btcutil.Amount(
		c.commitResolution.SelfOutputSignDesc.Output.Value,
	)

	// Set the initial report. All fields are filled in, except for the
	// maturity height which remains 0 until Resolve() is executed.
	//
	// TODO(joostjager): Resolvers only activate after the commit tx
	// confirms. With more refactoring in channel arbitrator, it would be
	// possible to make the confirmation height part of ResolverConfig and
	// populate MaturityHeight here.
	c.currentReport = ContractReport{
		Outpoint:         c.commitResolution.SelfOutPoint,
		Type:             ReportOutputUnencumbered,
		Amount:           amt,
		LimboBalance:     amt,
		RecoveredBalance: 0,
	}
}

// A compile time assertion to ensure commitSweepResolver meets the
// ContractResolver interface.
var _ reportingContractResolver = (*commitSweepResolver)(nil)
