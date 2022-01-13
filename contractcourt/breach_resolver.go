package contractcourt

import (
	"encoding/binary"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
)

// breachResolver is a resolver that will handle breached closes. In the
// future, this will likely take over the duties the current breacharbiter has.
type breachResolver struct {
	// resolved reflects if the contract has been fully resolved or not.
	resolved bool

	// subscribed denotes whether or not the breach resolver has subscribed
	// to the breacharbiter for breach resolution.
	subscribed bool

	// replyChan is closed when the breach arbiter has completed serving
	// justice.
	replyChan chan struct{}

	contractResolverKit
}

// newBreachResolver instantiates a new breach resolver.
func newBreachResolver(resCfg ResolverConfig) *breachResolver {
	r := &breachResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		replyChan:           make(chan struct{}),
	}

	r.initLogger(r)

	return r
}

// ResolverKey returns the unique identifier for this resolver.
func (b *breachResolver) ResolverKey() []byte {
	key := newResolverID(b.ChanPoint)
	return key[:]
}

// Resolve queries the breacharbiter to see if the justice transaction has been
// broadcast.
func (b *breachResolver) Resolve() (ContractResolver, error) {
	if !b.subscribed {
		complete, err := b.SubscribeBreachComplete(
			&b.ChanPoint, b.replyChan,
		)
		if err != nil {
			return nil, err
		}

		// If the breach resolution process is already complete, then
		// we can cleanup and checkpoint the resolved state.
		if complete {
			b.resolved = true
			return nil, b.Checkpoint(b)
		}

		// Prevent duplicate subscriptions.
		b.subscribed = true
	}

	select {
	case <-b.replyChan:
		// The replyChan has been closed, signalling that the breach
		// has been fully resolved. Checkpoint the resolved state and
		// exit.
		b.resolved = true
		return nil, b.Checkpoint(b)
	case <-b.quit:
	}

	return nil, errResolverShuttingDown
}

// Stop signals the breachResolver to stop.
func (b *breachResolver) Stop() {
	close(b.quit)
}

// IsResolved returns true if the breachResolver is fully resolved and cleanup
// can occur.
func (b *breachResolver) IsResolved() bool {
	return b.resolved
}

// SupplementState adds additional state to the breachResolver.
func (b *breachResolver) SupplementState(_ *channeldb.OpenChannel) {
}

// Encode encodes the breachResolver to the passed writer.
func (b *breachResolver) Encode(w io.Writer) error {
	return binary.Write(w, endian, b.resolved)
}

// newBreachResolverFromReader attempts to decode an encoded breachResolver
// from the passed Reader instance, returning an active breachResolver.
func newBreachResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*breachResolver, error) {

	b := &breachResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		replyChan:           make(chan struct{}),
	}

	if err := binary.Read(r, endian, &b.resolved); err != nil {
		return nil, err
	}

	b.initLogger(b)

	return b, nil
}

// A compile time assertion to ensure breachResolver meets the ContractResolver
// interface.
var _ ContractResolver = (*breachResolver)(nil)
