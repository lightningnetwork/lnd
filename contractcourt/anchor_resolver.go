package contractcourt

import (
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/sweep"
)

// anchorResolver is a resolver that will attempt to sweep our anchor output.
type anchorResolver struct {
	// anchorSignDescriptor contains the information that is required to
	// sweep the anchor.
	anchorSignDescriptor input.SignDescriptor

	// anchor is the outpoint on the commitment transaction.
	anchor wire.OutPoint

	// broadcastHeight is the height that the original contract was
	// broadcast to the main-chain at. We'll use this value to bound any
	// historical queries to the chain for spends/confirmations.
	broadcastHeight uint32

	// chanPoint is the channel point of the original contract.
	chanPoint wire.OutPoint

	// chanType denotes the type of channel the contract belongs to.
	chanType channeldb.ChannelType

	// currentReport stores the current state of the resolver for reporting
	// over the rpc interface.
	currentReport ContractReport

	// reportLock prevents concurrent access to the resolver report.
	reportLock sync.Mutex

	contractResolverKit
}

// newAnchorResolver instantiates a new anchor resolver.
func newAnchorResolver(anchorSignDescriptor input.SignDescriptor,
	anchor wire.OutPoint, broadcastHeight uint32,
	chanPoint wire.OutPoint, resCfg ResolverConfig) *anchorResolver {

	amt := btcutil.Amount(anchorSignDescriptor.Output.Value)

	report := ContractReport{
		Outpoint:         anchor,
		Type:             ReportOutputAnchor,
		Amount:           amt,
		LimboBalance:     amt,
		RecoveredBalance: 0,
	}

	r := &anchorResolver{
		contractResolverKit:  *newContractResolverKit(resCfg),
		anchorSignDescriptor: anchorSignDescriptor,
		anchor:               anchor,
		broadcastHeight:      broadcastHeight,
		chanPoint:            chanPoint,
		currentReport:        report,
	}

	r.initLogger(fmt.Sprintf("%T(%v)", r, r.anchor))

	return r
}

// ResolverKey returns an identifier which should be globally unique for this
// particular resolver within the chain the original contract resides within.
func (c *anchorResolver) ResolverKey() []byte {
	// The anchor resolver is stateless and doesn't need a database key.
	return nil
}

// Resolve waits for the output to be swept.
//
// NOTE: Part of the ContractResolver interface.
func (c *anchorResolver) Resolve() (ContractResolver, error) {
	// If we're already resolved, then we can exit early.
	if c.IsResolved() {
		c.log.Errorf("already resolved")
		return nil, nil
	}

	var (
		outcome channeldb.ResolverOutcome
		spendTx *chainhash.Hash
	)

	select {
	case sweepRes := <-c.sweepResultChan:
		err := sweepRes.Err

		switch {
		// Anchor was swept successfully.
		case err == nil:
			sweepTxID := sweepRes.Tx.TxHash()

			spendTx = &sweepTxID
			outcome = channeldb.ResolverOutcomeClaimed

		// Anchor was swept by someone else. This is possible after the
		// 16 block csv lock.
		case errors.Is(err, sweep.ErrRemoteSpend),
			errors.Is(err, sweep.ErrInputMissing):

			c.log.Warnf("our anchor spent by someone else")
			outcome = channeldb.ResolverOutcomeUnclaimed

		// An unexpected error occurred.
		default:
			c.log.Errorf("unable to sweep anchor: %v", sweepRes.Err)

			return nil, sweepRes.Err
		}

	case <-c.quit:
		return nil, errResolverShuttingDown
	}

	c.log.Infof("resolved in tx %v", spendTx)

	// Update report to reflect that funds are no longer in limbo.
	c.reportLock.Lock()
	if outcome == channeldb.ResolverOutcomeClaimed {
		c.currentReport.RecoveredBalance = c.currentReport.LimboBalance
	}
	c.currentReport.LimboBalance = 0
	report := c.currentReport.resolverReport(
		spendTx, channeldb.ResolverTypeAnchor, outcome,
	)
	c.reportLock.Unlock()

	c.markResolved()
	return nil, c.PutResolverReport(nil, report)
}

// Stop signals the resolver to cancel any current resolution processes, and
// suspend.
//
// NOTE: Part of the ContractResolver interface.
func (c *anchorResolver) Stop() {
	c.log.Debugf("stopping...")
	defer c.log.Debugf("stopped")

	close(c.quit)
}

// SupplementState allows the user of a ContractResolver to supplement it with
// state required for the proper resolution of a contract.
//
// NOTE: Part of the ContractResolver interface.
func (c *anchorResolver) SupplementState(state *channeldb.OpenChannel) {
	c.chanType = state.ChanType
}

// report returns a report on the resolution state of the contract.
func (c *anchorResolver) report() *ContractReport {
	c.reportLock.Lock()
	defer c.reportLock.Unlock()

	reportCopy := c.currentReport
	return &reportCopy
}

func (c *anchorResolver) Encode(w io.Writer) error {
	return errors.New("serialization not supported")
}

// A compile time assertion to ensure anchorResolver meets the
// ContractResolver interface.
var _ ContractResolver = (*anchorResolver)(nil)

// Launch offers the anchor output to the sweeper.
func (c *anchorResolver) Launch() error {
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

	// Attempt to update the sweep parameters to the post-confirmation
	// situation. We don't want to force sweep anymore, because the anchor
	// lost its special purpose to get the commitment confirmed. It is just
	// an output that we want to sweep only if it is economical to do so.
	//
	// An exclusive group is not necessary anymore, because we know that
	// this is the only anchor that can be swept. However, to avoid this
	// anchor input being group with other inputs, we still keep the
	// exclusive group here such that the anchor will be swept
	// independently.
	//
	// We also clear the parent tx information for cpfp, because the
	// commitment tx is confirmed.
	//
	// After a restart or when the remote force closes, the sweeper is not
	// yet aware of the anchor. In that case, it will be added as new input
	// to the sweeper.
	witnessType := input.CommitmentAnchor

	// For taproot channels, we need to use the proper witness type.
	if c.chanType.IsTaproot() {
		witnessType = input.TaprootAnchorSweepSpend
	}

	anchorInput := input.MakeBaseInput(
		&c.anchor, witnessType, &c.anchorSignDescriptor,
		c.broadcastHeight, nil,
	)

	exclusiveGroup := c.ShortChanID.ToUint64()

	resultChan, err := c.Sweeper.SweepInput(
		&anchorInput,
		sweep.Params{
			// For normal anchor sweeping, the budget is 330 sats.
			Budget: btcutil.Amount(
				anchorInput.SignDesc().Output.Value,
			),

			// There's no rush to sweep the anchor, so we use a nil
			// deadline here.
			DeadlineHeight: fn.None[int32](),

			// Use the chan id as the exclusive group. This prevents
			// any of the anchors from being batched together.
			ExclusiveGroup: &exclusiveGroup,
		},
	)

	if err != nil {
		return err
	}

	c.sweepResultChan = resultChan

	return nil
}
