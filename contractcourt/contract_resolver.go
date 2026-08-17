package contractcourt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/btcsuite/btcd/btcutil/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/sweep"
)

var (
	endian = binary.BigEndian
)

const (
	// sweepConfTarget is the default number of blocks that we'll use as a
	// confirmation target when sweeping.
	sweepConfTarget = 6
)

// ContractResolver is an interface which packages a state machine which is
// able to carry out the necessary steps required to fully resolve a Bitcoin
// contract on-chain. Resolvers are fully encodable to ensure callers are able
// to persist them properly. A resolver may produce another resolver in the
// case that claiming an HTLC is a multi-stage process. In this case, we may
// partially resolve the contract, then persist, and set up for an additional
// resolution.
type ContractResolver interface {
	// ResolverKey returns an identifier which should be globally unique
	// for this particular resolver within the chain the original contract
	// resides within.
	ResolverKey() []byte

	// Launch starts the resolver by constructing an input and offering it
	// to the sweeper. Once offered, it's expected to monitor the sweeping
	// result in a goroutine invoked by calling Resolve.
	//
	// NOTE: We can call `Resolve` inside a goroutine at the end of this
	// method to avoid calling it in the ChannelArbitrator. However, there
	// are some DB-related operations such as SwapContract/ResolveContract
	// which need to be done inside the resolvers instead, which needs a
	// deeper refactoring.
	Launch() error

	// Resolve instructs the contract resolver to resolve the output
	// on-chain. Once the output has been *fully* resolved, the function
	// should return immediately with a nil ContractResolver value for the
	// first return value.  In the case that the contract requires further
	// resolution, then another resolve is returned.
	//
	// NOTE: This function MUST be run as a goroutine.
	Resolve() (ContractResolver, error)

	// SupplementState allows the user of a ContractResolver to supplement
	// it with state required for the proper resolution of a contract.
	SupplementState(*chanstate.OpenChannel)

	// IsResolved returns true if the stored state in the resolve is fully
	// resolved. In this case the target output can be forgotten.
	IsResolved() bool

	// Encode writes an encoded version of the ContractResolver into the
	// passed Writer.
	Encode(w io.Writer) error

	// Stop signals the resolver to cancel any current resolution
	// processes, and suspend.
	Stop()
}

// htlcContractResolver is the required interface for htlc resolvers.
type htlcContractResolver interface {
	ContractResolver

	// HtlcPoint returns the htlc's outpoint on the commitment tx.
	HtlcPoint() wire.OutPoint

	// Supplement adds additional information to the resolver that is
	// required before Resolve() is called.
	Supplement(htlc channeldb.HTLC)

	// SupplementDeadline gives the deadline height for the HTLC output.
	// This is only useful for outgoing HTLCs.
	SupplementDeadline(deadlineHeight fn.Option[int32])
}

// reportingContractResolver is a ContractResolver that also exposes a report on
// the resolution state of the contract.
type reportingContractResolver interface {
	ContractResolver

	report() *ContractReport
}

// ResolverConfig contains the externally supplied configuration items that are
// required by a ContractResolver implementation.
type ResolverConfig struct {
	// ChannelArbitratorConfig contains all the interfaces and closures
	// required for the resolver to interact with outside sub-systems.
	ChannelArbitratorConfig

	// Checkpoint allows a resolver to check point its state. This function
	// should write the state of the resolver to persistent storage, and
	// return a non-nil error upon success. It takes a resolver report,
	// which contains information about the outcome and should be written
	// to disk if non-nil.
	Checkpoint func(ContractResolver, ...*channeldb.ResolverReport) error
}

// contractResolverKit is meant to be used as a mix-in struct to be embedded within a
// given ContractResolver implementation. It contains all the common items that
// a resolver requires to carry out its duties.
type contractResolverKit struct {
	ResolverConfig

	log btclog.Logger

	quit chan struct{}

	// sweepResultChan is the result chan returned from calling
	// `SweepInput`. It should be mounted to the specific resolver once the
	// input has been offered to the sweeper.
	sweepResultChan chan sweep.Result

	// launched specifies whether the resolver has been launched. Calling
	// `Launch` will be a no-op if this is true. This value is not saved to
	// db, as it's fine to relaunch a resolver after a restart. It's only
	// used to avoid resending requests to the sweeper when a new blockbeat
	// is received.
	launched atomic.Bool

	// resolved reflects if the contract has been fully resolved or not.
	resolved atomic.Bool

	// wg tracks background goroutines spawned by the resolver (async
	// pre-signed tx publishes and anchor sweep result consumers) so Stop
	// can wait for them to exit.
	wg sync.WaitGroup
}

// newContractResolverKit instantiates the mix-in struct.
func newContractResolverKit(cfg ResolverConfig) *contractResolverKit {
	return &contractResolverKit{
		ResolverConfig: cfg,
		quit:           make(chan struct{}),
	}
}

// initLogger initializes the resolver-specific logger.
func (r *contractResolverKit) initLogger(prefix string) {
	logPrefix := fmt.Sprintf("ChannelArbitrator(%v): %s:", r.ChanPoint,
		prefix)

	r.log = log.WithPrefix(logPrefix)
}

// IsResolved returns true if the stored state in the resolve is fully
// resolved. In this case the target output can be forgotten.
//
// NOTE: Part of the ContractResolver interface.
func (r *contractResolverKit) IsResolved() bool {
	return r.resolved.Load()
}

// markResolved marks the resolver as resolved.
func (r *contractResolverKit) markResolved() {
	r.resolved.Store(true)
}

// isLaunched returns true if the resolver has been launched.
func (r *contractResolverKit) isLaunched() bool {
	return r.launched.Load()
}

// markLaunched marks the resolver as launched.
func (r *contractResolverKit) markLaunched() {
	r.launched.Store(true)
}

var (
	// errResolverShuttingDown is returned when the resolver stops
	// progressing because it received the quit signal.
	errResolverShuttingDown = errors.New("resolver shutting down")
)

// publishPreSignedHtlcTx broadcasts a pre-signed second-level HTLC
// transaction asynchronously. The transaction may carry an absolute locktime
// (timeout txs), in which case it is not final until the chain reaches that
// height and the mempool would reject it. We therefore wait for block epochs
// and publish once the locktime is satisfiable, retrying on every new block
// until the broadcast succeeds (transient rejections such as a not-yet-final
// locktime or a momentary mempool conflict resolve themselves as the chain
// advances; an exact duplicate of an already-known transaction is treated as
// success by PublishTx). Once the broadcast succeeds, onPublished is invoked
// (used to offer the tx's CPFP anchor to the sweeper).
//
// The resolver's Resolve() path independently waits for the spend of the
// HTLC output, so running the broadcast asynchronously does not change
// resolution semantics.
func publishPreSignedHtlcTx(tx *wire.MsgTx, label string,
	publish func(*wire.MsgTx, string) error,
	notifier chainntnfs.ChainNotifier, quit <-chan struct{},
	wg *sync.WaitGroup, log btclog.Logger, onPublished func() error) error {

	epochClient, err := notifier.RegisterBlockEpochNtfn(nil)
	if err != nil {
		return fmt.Errorf("register block epochs: %w", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer epochClient.Cancel()

		for {
			select {
			case epoch, ok := <-epochClient.Epochs:
				if !ok {
					return
				}

				// A locktime'd tx is only final once the next
				// block's height exceeds the locktime, i.e.
				// the current height reached it.
				if uint32(epoch.Height) < tx.LockTime {
					continue
				}

			case <-quit:
				return
			}

			if err := publish(tx, label); err != nil {
				log.Warnf("unable to publish pre-signed "+
					"second-level tx=%v, retrying on "+
					"next block: %v", tx.TxHash(), err)

				continue
			}

			log.Infof("published pre-signed second-level tx=%v",
				tx.TxHash())

			if onPublished != nil {
				if err := onPublished(); err != nil {
					log.Errorf("post-publish handling "+
						"for tx=%v failed: %v",
						tx.TxHash(), err)
				}
			}

			return
		}
	}()

	return nil
}

// secondLevelAnchorSweepReq bundles everything needed to offer the CPFP
// anchor of a pre-signed second-level HTLC tx to the sweeper.
type secondLevelAnchorSweepReq struct {
	// sweeper is the sweeper the anchor input is handed to.
	sweeper UtxoSweeper

	// parentTx is the just-broadcast pre-signed second-level HTLC tx
	// carrying the anchor at output index 1.
	parentTx *wire.MsgTx

	// htlcSweepDesc is the second-level HTLC output's sweep sign
	// descriptor, carrying the delay key material the anchor is keyed to.
	htlcSweepDesc input.SignDescriptor

	// parentFee is the exact fee baked into the parent tx, i.e. the
	// value of the commitment output it spends minus the sum of its
	// outputs.
	parentFee btcutil.Amount

	// budget is the node's sweeper budget configuration, used to derive
	// the fee budget for the CPFP child from the value under protection.
	budget BudgetConfig

	// broadcastHeight is the height the parent tx was broadcast at.
	broadcastHeight uint32

	// deadlineHeight is the height the second-level tx must confirm by,
	// if any: the incoming HTLC expiry for the timeout path, the HTLC's
	// own expiry for the success path.
	deadlineHeight fn.Option[int32]

	// quit is closed when the owning resolver shuts down.
	quit <-chan struct{}

	// wg tracks the background goroutine consuming the sweep result.
	wg *sync.WaitGroup

	log btclog.Logger
}

// offerSecondLevelAnchorToSweeper offers the anchor output at index 1 of
// a just-broadcast DeterministicHTLCs second-level HTLC tx to the sweeper
// for CPFP fee bumping. The pre-signed parent tx cannot be RBF'd under
// SigHashDefault, so CPFP via this anchor is the only fee-bumping path.
//
// The anchor is a taproot output spendable by the broadcaster's
// ToLocalKey via key-path (fast) or anyone after 16 blocks via script
// path (fallback). We use the key-path here. The ToLocalKey is sourced
// from the stored ResolveReq.KeyRing; without it we fall back to a
// no-op (CPFP unavailable, parent tx still publishes at its baked-in
// floor fee).
func offerSecondLevelAnchorToSweeper(req *secondLevelAnchorSweepReq) error {
	sweeper := req.sweeper
	parentTx := req.parentTx
	htlcSweepDesc := req.htlcSweepDesc
	log := req.log

	// The anchor sits at index 1 of every DeterministicHTLCs second-level
	// tx (index 0 is the HTLC output). If the tx only has one output the
	// caller built it without an anchor and there's nothing to sweep.
	if len(parentTx.TxOut) < 2 {
		return nil
	}

	// The anchor is keyed to the broadcaster's to-local delay key, which
	// is exactly the key the second-level HTLC output's sweep descriptor
	// signs with: the delay base point tweaked with the commitment
	// point's single tweak. Derive it from the descriptor so this path
	// is fully self-contained.
	if htlcSweepDesc.KeyDesc.PubKey == nil ||
		len(htlcSweepDesc.SingleTweak) == 0 {

		log.Warnf("cannot sweep second-level anchor: sweep sign " +
			"descriptor lacks delay key material; CPFP " +
			"unavailable")

		return nil
	}
	delayKey := input.TweakPubKeyWithTweak(
		htlcSweepDesc.KeyDesc.PubKey, htlcSweepDesc.SingleTweak,
	)
	anchorTree, err := input.NewAnchorScriptTree(delayKey)
	if err != nil {
		return fmt.Errorf("build anchor script tree: %w", err)
	}

	op := wire.OutPoint{
		Hash:  parentTx.TxHash(),
		Index: 1,
	}

	// Build the sign descriptor for the key-path spend. Clone the HTLC
	// output's SweepSignDesc (which already carries the local delay key
	// descriptor + tweak) and swap the anchor-specific fields.
	signDesc := htlcSweepDesc
	signDesc.Output = parentTx.TxOut[1]
	signDesc.WitnessScript = anchorTree.SweepLeaf.Script
	signDesc.TapTweak = anchorTree.TapscriptRoot
	signDesc.HashType = txscript.SigHashDefault

	// We pass the parent tx fee + weight so the sweeper computes the
	// effective package fee rate for CPFP. The baked-in parent fee is
	// the floor rate (see lnwallet.HtlcSuccessFee / HtlcTimeoutFee under
	// sigHashDefault) and the weight includes the appended anchor.
	strippedSize := parentTx.SerializeSizeStripped()
	parentWeight := lntypes.WeightUnit(
		(strippedSize * 3) + parentTx.SerializeSize(),
	)
	parentInfo := &input.TxInfo{
		Fee:    req.parentFee,
		Weight: parentWeight,
	}

	anchorInput := input.MakeBaseInput(
		&op, input.TaprootAnchorSweepSpend, &signDesc,
		req.broadcastHeight, parentInfo,
	)

	// The budget bounds the fees the sweeper may pay for the CPFP child
	// (funded from wallet inputs, so it can and usually must exceed the
	// anchor's own value). Derive it from the value under protection,
	// the second-level HTLC output, with the same configuration used
	// for commitment anchor CPFP, plus the anchor value itself.
	budget := calculateBudget(
		btcutil.Amount(parentTx.TxOut[0].Value),
		req.budget.AnchorCPFPRatio, req.budget.AnchorCPFP,
	) + AnchorOutputValue

	resultChan, err := sweeper.SweepInput(&anchorInput, sweep.Params{
		Budget:         budget,
		DeadlineHeight: req.deadlineHeight,
	})
	if err != nil {
		return fmt.Errorf("offer second-level anchor: %w", err)
	}

	// The sweep can still fail terminally after being accepted (budget
	// exhausted, deadline blown, persistent fee estimation failure).
	// There's no corrective action to take, the parent may still confirm
	// at its baked-in fee, but the outcome must not vanish silently.
	req.wg.Add(1)
	go func() {
		defer req.wg.Done()

		select {
		case result, ok := <-resultChan:
			if !ok {
				return
			}
			if result.Err != nil {
				log.Errorf("second-level anchor=%v CPFP "+
					"sweep failed: %v", op, result.Err)

				return
			}

			if result.Tx != nil {
				log.Infof("second-level anchor=%v swept by "+
					"tx=%v", op, result.Tx.TxHash())
			}

		case <-req.quit:
		}
	}()

	log.Infof("offered second-level anchor=%v to sweeper for CPFP "+
		"(budget=%v, deadline=%v)", op, budget, req.deadlineHeight)

	return nil
}

// parentTxFee returns the fee paid by the given tx, computed as the
// difference between input and output value. The caller is responsible
// for ensuring the inputs' prevout values are known via context; here
// we approximate using the tx outputs since the only prevout is the
// HTLC output of the commitment tx and that value isn't reachable
// without extra plumbing. For floor-rate-signed second-level txs the
// approximation is close enough for the sweeper's package-fee math.
// preSignedTxFee returns the exact fee baked into a pre-signed second-level
// HTLC tx: the value of the commitment output it spends (carried by the sign
// details' sign descriptor) minus the sum of its outputs.
func preSignedTxFee(tx *wire.MsgTx,
	signDetails *input.SignDetails) btcutil.Amount {

	inputValue := signDetails.SignDesc.Output.Value

	var outputValue int64
	for _, txOut := range tx.TxOut {
		outputValue += txOut.Value
	}

	return btcutil.Amount(inputValue - outputValue)
}

// isSecondLevelSigHashDefault returns true when a pre-signed second-level
// HTLC transaction was signed with SigHashDefault. In this case the tx
// has baked-in fees and must be broadcast as-is: the sweeper cannot add
// wallet inputs or change outputs without invalidating the peer's
// signature.
//
// The sighash check alone filters every channel that populates sign
// details today: legacy channels sign second levels with SIGHASH_ALL
// (0x01), anchor and taproot channels with SIGHASH_SINGLE|ANYONECANPAY
// (0x83), and taproot asset channels that did not negotiate
// DeterministicHTLCs also carry 0x83. The channel type gate
// (TapscriptRootBit, only ever set for aux/custom channels) is layered
// on top for two reasons: it makes the custom-channel-only isolation of
// this path explicit, and it protects against SigHashDefault being the
// zero value of SigHashType, so no present or future code path that
// leaves the field unset can ever steer a non-custom channel in here.
func isSecondLevelSigHashDefault(signDetails *input.SignDetails,
	chanType channeldb.ChannelType) bool {

	return signDetails != nil &&
		signDetails.SigHashType == txscript.SigHashDefault &&
		chanType.HasTapscriptRoot()
}
