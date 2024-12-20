package contractcourt

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/channeldb"
)

// breachResolver is a resolver that will handle breached closes. In the
// future, this will likely take over the duties the current BreachArbitrator
// has.
type breachResolver struct {
	// subscribed denotes whether or not the breach resolver has subscribed
	// to the BreachArbitrator for breach resolution.
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

	r.initLogger(fmt.Sprintf("%T(%v)", r, r.ChanPoint))

	return r
}

// ResolverKey returns the unique identifier for this resolver.
func (b *breachResolver) ResolverKey() []byte {
	key := newResolverID(b.ChanPoint)
	return key[:]
}

// Resolve queries the BreachArbitrator to see if the justice transaction has
// been broadcast.
//
// NOTE: Part of the ContractResolver interface.
//
// TODO(yy): let sweeper handle the breach inputs.
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
			b.markResolved()
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
		b.markResolved()
		return nil, b.Checkpoint(b)

	case <-b.quit:
	}

	return nil, errResolverShuttingDown
}

// Stop signals the breachResolver to stop.
func (b *breachResolver) Stop() {
	b.log.Debugf("stopping...")
	close(b.quit)
}

// SupplementState adds additional state to the breachResolver.
func (b *breachResolver) SupplementState(_ *channeldb.OpenChannel) {
}

// Encode encodes the breachResolver to the passed writer.
func (b *breachResolver) Encode(w io.Writer) error {
	return binary.Write(w, endian, b.IsResolved())
}

// newBreachResolverFromReader attempts to decode an encoded breachResolver
// from the passed Reader instance, returning an active breachResolver.
func newBreachResolverFromReader(r io.Reader, resCfg ResolverConfig) (
	*breachResolver, error) {

	b := &breachResolver{
		contractResolverKit: *newContractResolverKit(resCfg),
		replyChan:           make(chan struct{}),
	}

	var resolved bool
	if err := binary.Read(r, endian, &resolved); err != nil {
		return nil, err
	}
	if resolved {
		b.markResolved()
	}

	b.initLogger(fmt.Sprintf("%T(%v)", b, b.ChanPoint))

	return b, nil
}

// A compile time assertion to ensure breachResolver meets the ContractResolver
// interface.
var _ ContractResolver = (*breachResolver)(nil)

// Launch offers the breach outputs to the sweeper - currently it's a NOOP as
// the outputs here are not offered to the sweeper.
//
// NOTE: Part of the ContractResolver interface.
//
// TODO(yy): implement it once the outputs are offered to the sweeper.
func (b *breachResolver) Launch() error {
	if b.isLaunched() {
		b.log.Tracef("already launched")
		return nil
	}

	b.log.Debugf("launching resolver...")
	b.markLaunched()

	return nil
}
