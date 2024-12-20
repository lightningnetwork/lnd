package contractcourt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
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
	SupplementState(*channeldb.OpenChannel)

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
