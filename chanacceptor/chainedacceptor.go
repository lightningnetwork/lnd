package chanacceptor

import (
	"sync"
	"sync/atomic"
)

// ChainedAcceptor represents a conjunction of ChannelAcceptor results.
type ChainedAcceptor struct {
	// acceptors is a map of ChannelAcceptors that will be evaluated when
	// the ChainedAcceptor's Accept method is called.
	acceptors    map[uint64]ChannelAcceptor
	acceptorsMtx sync.RWMutex

	acceptorID uint64 // To be used atomically.
}

// NewChainedAcceptor initializes a ChainedAcceptor.
func NewChainedAcceptor() *ChainedAcceptor {
	return &ChainedAcceptor{
		acceptors: make(map[uint64]ChannelAcceptor),
	}
}

// AddAcceptor adds a ChannelAcceptor to this ChainedAcceptor.
func (c *ChainedAcceptor) AddAcceptor(acceptor ChannelAcceptor) uint64 {
	id := atomic.AddUint64(&c.acceptorID, 1)

	c.acceptorsMtx.Lock()
	c.acceptors[id] = acceptor
	c.acceptorsMtx.Unlock()

	// Return the id so that a caller can call RemoveAcceptor.
	return id
}

// RemoveAcceptor removes a ChannelAcceptor from this ChainedAcceptor given
// an ID.
func (c *ChainedAcceptor) RemoveAcceptor(id uint64) {
	c.acceptorsMtx.Lock()
	delete(c.acceptors, id)
	c.acceptorsMtx.Unlock()
}

// Accept evaluates the results of all ChannelAcceptors in the acceptors map
// and returns the conjunction of all these predicates.
//
// NOTE: Part of the ChannelAcceptor interface.
func (c *ChainedAcceptor) Accept(req *ChannelAcceptRequest) bool {
	result := true

	c.acceptorsMtx.RLock()
	for _, acceptor := range c.acceptors {
		// We call Accept first in case any acceptor (perhaps an RPCAcceptor)
		// wishes to be notified about ChannelAcceptRequest.
		result = acceptor.Accept(req) && result
	}
	c.acceptorsMtx.RUnlock()

	return result
}

// A compile-time constraint to ensure ChainedAcceptor implements the
// ChannelAcceptor interface.
var _ ChannelAcceptor = (*ChainedAcceptor)(nil)
