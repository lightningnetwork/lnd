package contractcourt

import (
	"encoding/binary"
	"io"
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

	// Decode attempts to decode an encoded ContractResolver from the
	// passed Reader instance, returning an active ContractResolver
	// instance.
	Decode(r io.Reader) error

	// AttachResolverKit should be called once a resolved is successfully
	// decoded from its stored format. This struct delivers a generic tool
	// kit that resolvers need to complete their duty.
	AttachResolverKit(ResolverKit)

	// Stop signals the resolver to cancel any current resolution
	// processes, and suspend.
	Stop()
}

// ResolverKit is meant to be used as a mix-in struct to be embedded within a
// given ContractResolver implementation. It contains all the items that a
// resolver requires to carry out its duties.
type ResolverKit struct {
	// ChannelArbitratorConfig contains all the interfaces and closures
	// required for the resolver to interact with outside sub-systems.
	ChannelArbitratorConfig

	// Checkpoint allows a resolver to check point its state. This function
	// should write the state of the resolver to persistent storage, and
	// return a non-nil error upon success.
	Checkpoint func(ContractResolver) error

	Quit chan struct{}
}
