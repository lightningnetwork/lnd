package chanacceptor

import (
	"sync"
	"sync/atomic"
)

// ChainedAcceptor represents a conjunction of ChannelAcceptor results.
type ChainedAcceptor struct {
	acceptorID uint64 // To be used atomically.

	// acceptors is a map of ChannelAcceptors that will be evaluated when
	// the ChainedAcceptor's Accept method is called.
	acceptors    map[uint64]ChannelAcceptor
	acceptorsMtx sync.RWMutex
}

// NewChainedAcceptor initializes a ChainedAcceptor.
func NewChainedAcceptor() *ChainedAcceptor {
	return &ChainedAcceptor{
		acceptors: make(map[uint64]ChannelAcceptor),
	}
}

// AddAcceptor adds a ChannelAcceptor to this ChainedAcceptor.
//
// NOTE: Part of the MultiplexAcceptor interface.
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
//
// NOTE: Part of the MultiplexAcceptor interface.
func (c *ChainedAcceptor) RemoveAcceptor(id uint64) {
	c.acceptorsMtx.Lock()
	delete(c.acceptors, id)
	c.acceptorsMtx.Unlock()
}

// numAcceptors returns the number of acceptors contained in the
// ChainedAcceptor.
func (c *ChainedAcceptor) numAcceptors() int {
	c.acceptorsMtx.RLock()
	defer c.acceptorsMtx.RUnlock()
	return len(c.acceptors)
}

// Accept evaluates the results of all ChannelAcceptors in the acceptors map
// and returns the conjunction of all these predicates.
//
// NOTE: Part of the ChannelAcceptor interface.
func (c *ChainedAcceptor) Accept(req *ChannelAcceptRequest) *ChannelAcceptResponse {
	c.acceptorsMtx.RLock()
	defer c.acceptorsMtx.RUnlock()

	var finalResp ChannelAcceptResponse

	for _, acceptor := range c.acceptors {
		// Call our acceptor to determine whether we want to accept this
		// channel.
		acceptorResponse := acceptor.Accept(req)

		// If we should reject the channel, we can just exit early. This
		// has the effect of returning the error belonging to our first
		// failed acceptor.
		if acceptorResponse.RejectChannel() {
			return acceptorResponse
		}

		// If we have accepted the channel, we need to set the other
		// fields that were set in the response. However, since we are
		// dealing with multiple responses, we need to make sure that we
		// have not received inconsistent values (eg a csv delay of 1
		// from one acceptor, and a delay of 120 from another). We
		// set each value on our final response if it has not been set
		// yet, and allow duplicate sets if the value is the same. If
		// we cannot set a field, we return an error response.
		var err error
		finalResp, err = mergeResponse(finalResp, *acceptorResponse)
		if err != nil {
			log.Errorf("response for: %x has inconsistent values: %v",
				req.OpenChanMsg.PendingChannelID, err)

			return NewChannelAcceptResponse(
				false, errChannelRejected, nil, 0, 0,
				0, 0, 0, 0, false,
			)
		}
	}

	// If we have gone through all of our acceptors with no objections, we
	// can return an acceptor with a nil error.
	return &finalResp
}

// A compile-time constraint to ensure ChainedAcceptor implements the
// MultiplexAcceptor interface.
var _ MultiplexAcceptor = (*ChainedAcceptor)(nil)
