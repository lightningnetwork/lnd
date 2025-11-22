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
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/sweep"
)

// commitSweepResolver is a resolver that will attempt to sweep the commitment
// output paying to us (local channel balance). In the case that the local
// party (we) broadcasts their version of the commitment transaction, we have
// to wait before sweeping it, as it has a CSV delay. For anchor channel
// type, even if the remote party broadcasts the commitment transaction,
// we have to wait one block after commitment transaction is confirmed,
// because CSV 1 is put into the script of UTXO representing local balance.
// Additionally, if the channel is a channel lease, we have to wait for
// CLTV to expire.
// https://docs.lightning.engineering/lightning-network-tools/pool/overview
type commitSweepResolver struct {
	// localChanCfg is used to provide the resolver with the keys required
	// to identify whether the commitment transaction was broadcast by the
	// local or remote party.
	localChanCfg channeldb.ChannelConfig

	// commitResolution contains all data required to successfully sweep
	// this HTLC on-chain.
	commitResolution lnwallet.CommitOutputResolution

	// confirmHeight is the block height that the commitment transaction was
	// confirmed at. We'll use this value to bound any historical queries to
	// the chain for spends/confirmations.
	confirmHeight uint32

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
	confirmHeight uint32, chanPoint wire.OutPoint,
	resCfg ResolverConfig) *commitSweepResolver {

	r := &commitSweepResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		commitResolution:    res,
		confirmHeight:       confirmHeight,
		chanPoint:           chanPoint,
	}

	r.initLogger(fmt.Sprintf("%T(%v)", r, r.commitResolution.SelfOutPoint))
	r.initReport()

	return r
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
func (c *commitSweepResolver) ResolverKey() []byte {
	key := newResolverID(c.commitResolution.SelfOutPoint)
	return key[:]
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

// Resolve instructs the contract resolver to resolve the output on-chain. Once
// the output has been *fully* resolved, the function should return immediately
// with a nil ContractResolver value for the first return value.  In the case
// that the contract requires further resolution, then another resolve is
// returned.
//
// NOTE: This function MUST be run as a goroutine.

// TODO(yy): fix the funlen in the next PR.
//
//nolint:funlen
func (c *commitSweepResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if c.IsResolved() {
		c.log.Errorf("already resolved")
		return nil, nil
	}

	var sweepTxID chainhash.Hash

	// Sweeper is going to join this input with other inputs if possible
	// and publish the sweep tx. When the sweep tx confirms, it signals us
	// through the result channel with the outcome. Wait for this to
	// happen.
	outcome := channeldb.ResolverOutcomeClaimed
	select {
	case sweepResult := <-c.sweepResultChan:
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
	c.markResolved()

	// Checkpoint the resolver with a closure that will write the outcome
	// of the resolver and its sweep transaction to disk.
	return nil, c.Checkpoint(c, report)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) Stop() {
	c.log.Debugf("stopping...")
	defer c.log.Debugf("stopped")
	close(c.quit)
}

// SupplementState allows the user of a ContractResolver to supplement it with
// state required for the proper resolution of a contract.
//
// NOTE: Part of the ContractResolver interface.
func (c *commitSweepResolver) SupplementState(state *channeldb.OpenChannel) {
	if state.ChanType.HasLeaseExpiration() {
		c.leaseExpiry = state.ThawHeight
	}
	c.localChanCfg = state.LocalChanCfg
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

	if err := binary.Write(w, endian, c.IsResolved()); err != nil {
		return err
	}
	if err := binary.Write(w, endian, c.confirmHeight); err != nil {
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

	var resolved bool
	if err := binary.Read(r, endian, &resolved); err != nil {
		return nil, err
	}
	if resolved {
		c.markResolved()
	}

	if err := binary.Read(r, endian, &c.confirmHeight); err != nil {
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

	c.initLogger(fmt.Sprintf("%T(%v)", c, c.commitResolution.SelfOutPoint))
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

// Launch constructs a commit input and offers it to the sweeper.
func (c *commitSweepResolver) Launch() error {
	if c.isLaunched() {
		c.log.Tracef("already launched")
		return nil
	}

	c.log.Debugf("launching resolver...")
	c.markLaunched()

	// If we're already resolved, then we can exit early.
	if c.IsResolved() {
		c.log.Errorf("already resolved")
		return nil
	}

	// Wait up until the CSV expires, unless we also have a CLTV that
	// expires after.
	unlockHeight := c.confirmHeight + c.commitResolution.MaturityDelay
	if c.hasCLTV() {
		unlockHeight = max(unlockHeight, c.leaseExpiry)
	}

	// Update report with the calculated maturity height.
	c.reportLock.Lock()
	c.currentReport.MaturityHeight = unlockHeight
	c.reportLock.Unlock()

	// Derive the witness type for this input.
	witnessType, err := c.decideWitnessType()
	if err != nil {
		return err
	}

	// We'll craft an input with all the information required for the
	// sweeper to create a fully valid sweeping transaction to recover
	// these coins.
	var inp *input.BaseInput
	if c.hasCLTV() {
		inp = input.NewCsvInputWithCltv(
			&c.commitResolution.SelfOutPoint, witnessType,
			&c.commitResolution.SelfOutputSignDesc,
			c.confirmHeight, c.commitResolution.MaturityDelay,
			c.leaseExpiry, input.WithResolutionBlob(
				c.commitResolution.ResolutionBlob,
			),
		)
	} else {
		inp = input.NewCsvInput(
			&c.commitResolution.SelfOutPoint, witnessType,
			&c.commitResolution.SelfOutputSignDesc,
			c.confirmHeight, c.commitResolution.MaturityDelay,
			input.WithResolutionBlob(
				c.commitResolution.ResolutionBlob,
			),
		)
	}

	// TODO(roasbeef): instead of adding ctrl block to the sign desc, make
	// new input type, have sweeper set it?

	// Calculate the budget for the sweeping this input.
	budget := calculateBudget(
		btcutil.Amount(inp.SignDesc().Output.Value),
		c.Budget.ToLocalRatio, c.Budget.ToLocal,
	)
	c.log.Infof("sweeping commit output %v using budget=%v", witnessType,
		budget)

	// With our input constructed, we'll now offer it to the sweeper.
	resultChan, err := c.Sweeper.SweepInput(
		inp, sweep.Params{
			Budget: budget,

			// Specify a nil deadline here as there's no time
			// pressure.
			DeadlineHeight: fn.None[int32](),
		},
	)
	if err != nil {
		c.log.Errorf("unable to sweep input: %v", err)

		return err
	}

	c.sweepResultChan = resultChan

	return nil
}

// decideWitnessType returns the witness type for the input.
func (c *commitSweepResolver) decideWitnessType() (input.WitnessType, error) {
	var (
		isLocalCommitTx bool
		signDesc        = c.commitResolution.SelfOutputSignDesc
	)

	switch {
	// For taproot channels, we'll know if this is the local commit based
	// on the timelock value. For remote commitment transactions, the
	// witness script has a timelock of 1.
	case c.chanType.IsTaproot():
		delayKey := c.localChanCfg.DelayBasePoint.PubKey
		nonDelayKey := c.localChanCfg.PaymentBasePoint.PubKey

		signKey := c.commitResolution.SelfOutputSignDesc.KeyDesc.PubKey

		// If the key in the script is neither of these, we shouldn't
		// proceed. This should be impossible.
		if !signKey.IsEqual(delayKey) && !signKey.IsEqual(nonDelayKey) {
			return nil, fmt.Errorf("unknown sign key %v", signKey)
		}

		// The commitment transaction is ours iff the signing key is
		// the delay key.
		isLocalCommitTx = signKey.IsEqual(delayKey)

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

	return witnessType, nil
}
