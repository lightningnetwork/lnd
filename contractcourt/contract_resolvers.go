package contractcourt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btclog"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/channeldb"
)

var (
	endian = binary.BigEndian
)

const (
	// sweepConfTarget is the default number of blocks that we'll use as a
	// confirmation target when sweeping.
	sweepConfTarget = 6

	// secondLevelConfTarget is the confirmation target we'll use when
	// adding fees to our second-level HTLC transactions.
	secondLevelConfTarget = 6
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

	// Resolve instructs the contract resolver to resolve the output
	// on-chain. Once the output has been *fully* resolved, the function
	// should return immediately with a nil ContractResolver value for the
	// first return value.  In the case that the contract requires further
	// resolution, then another resolve is returned.
	//
	// NOTE: This function MUST be run as a goroutine.
	Resolve() (ContractResolver, error)

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
}

// newContractResolverKit instantiates the mix-in struct.
func newContractResolverKit(cfg ResolverConfig) *contractResolverKit {
	return &contractResolverKit{
		ResolverConfig: cfg,
		quit:           make(chan struct{}),
	}
}

// initLogger initializes the resolver-specific logger.
func (r *contractResolverKit) initLogger(resolver ContractResolver) {
	logPrefix := fmt.Sprintf("%T(%v):", resolver, r.ChanPoint)
	r.log = build.NewPrefixLog(logPrefix, log)
}

var (
	// errResolverShuttingDown is returned when the resolver stops
	// progressing because it received the quit signal.
	errResolverShuttingDown = errors.New("resolver shutting down")
)
