package routing

import (
	"errors"
	"sync"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// ErrVBarrierShuttingDown signals that the barrier has been requested to
// shutdown, and that the caller should not treat the wait condition as
// fulfilled.
var ErrVBarrierShuttingDown = errors.New("validation barrier shutting down")

// ValidationBarrier is a barrier used to ensure proper validation order while
// concurrently validating new announcements for channel edges, and the
// attributes of channel edges.  It uses this set of maps (protected by this
// mutex) to track validation dependencies. For a given channel our
// dependencies look like this: chanAnn <- chanUp <- nodeAnn. That is we must
// validate the item on the left of the arrow before that on the right.
type ValidationBarrier struct {
	// validationSemaphore is a channel of structs which is used as a
	// sempahore. Initially we'll fill this with a buffered channel of the
	// size of the number of active requests. Each new job will consume
	// from this channel, then restore the value upon completion.
	validationSemaphore chan struct{}

	// chanAnnFinSignal is map that keep track of all the pending
	// ChannelAnnouncement like validation job going on. Once the job has
	// been completed, the channel will be closed unblocking any
	// dependants.
	chanAnnFinSignal map[lnwire.ShortChannelID]chan struct{}

	// chanEdgeDependencies tracks any channel edge updates which should
	// wait until the completion of the ChannelAnnouncement before
	// proceeding. This is a dependency, as we can't validate the update
	// before we validate the announcement which creates the channel
	// itself.
	chanEdgeDependencies map[lnwire.ShortChannelID]chan struct{}

	// nodeAnnDependencies tracks any pending NodeAnnouncement validation
	// jobs which should wait until the completion of the
	// ChannelAnnouncement before proceeding.
	nodeAnnDependencies map[route.Vertex]chan struct{}

	quit chan struct{}
	sync.Mutex
}

// NewValidationBarrier creates a new instance of a validation barrier given
// the total number of active requests, and a quit channel which will be used
// to know when to kill pending, but unfilled jobs.
func NewValidationBarrier(numActiveReqs int,
	quitChan chan struct{}) *ValidationBarrier {

	v := &ValidationBarrier{
		chanAnnFinSignal:     make(map[lnwire.ShortChannelID]chan struct{}),
		chanEdgeDependencies: make(map[lnwire.ShortChannelID]chan struct{}),
		nodeAnnDependencies:  make(map[route.Vertex]chan struct{}),
		quit:                 quitChan,
	}

	// We'll first initialize a set of sempahores to limit our concurrency
	// when validating incoming requests in parallel.
	v.validationSemaphore = make(chan struct{}, numActiveReqs)
	for i := 0; i < numActiveReqs; i++ {
		v.validationSemaphore <- struct{}{}
	}

	return v
}

// InitJobDependencies will wait for a new job slot to become open, and then
// sets up any dependent signals/trigger for the new job
func (v *ValidationBarrier) InitJobDependencies(job interface{}) {
	// We'll wait for either a new slot to become open, or for the quit
	// channel to be closed.
	select {
	case <-v.validationSemaphore:
	case <-v.quit:
	}

	v.Lock()
	defer v.Unlock()

	// Once a slot is open, we'll examine the message of the job, to see if
	// there need to be any dependent barriers set up.
	switch msg := job.(type) {

	// If this is a channel announcement, then we'll need to set up den
	// tenancies, as we'll need to verify this before we verify any
	// ChannelUpdates for the same channel, or NodeAnnouncements of nodes
	// that are involved in this channel. This goes for both the wire
	// type,s and also the types that we use within the database.
	case *lnwire.ChannelAnnouncement:

		// We ensure that we only create a new announcement signal iff,
		// one doesn't already exist, as there may be duplicate
		// announcements.  We'll close this signal once the
		// ChannelAnnouncement has been validated. This will result in
		// all the dependent jobs being unlocked so they can finish
		// execution themselves.
		if _, ok := v.chanAnnFinSignal[msg.ShortChannelID]; !ok {
			// We'll create the channel that we close after we
			// validate this announcement. All dependants will
			// point to this same channel, so they'll be unblocked
			// at the same time.
			annFinCond := make(chan struct{})
			v.chanAnnFinSignal[msg.ShortChannelID] = annFinCond
			v.chanEdgeDependencies[msg.ShortChannelID] = annFinCond

			v.nodeAnnDependencies[route.Vertex(msg.NodeID1)] = annFinCond
			v.nodeAnnDependencies[route.Vertex(msg.NodeID2)] = annFinCond
		}
	case *channeldb.ChannelEdgeInfo:

		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		if _, ok := v.chanAnnFinSignal[shortID]; !ok {
			annFinCond := make(chan struct{})

			v.chanAnnFinSignal[shortID] = annFinCond
			v.chanEdgeDependencies[shortID] = annFinCond

			v.nodeAnnDependencies[route.Vertex(msg.NodeKey1Bytes)] = annFinCond
			v.nodeAnnDependencies[route.Vertex(msg.NodeKey2Bytes)] = annFinCond
		}

	// These other types don't have any dependants, so no further
	// initialization needs to be done beyond just occupying a job slot.
	case *channeldb.ChannelEdgePolicy:
		return
	case *lnwire.ChannelUpdate:
		return
	case *lnwire.NodeAnnouncement:
		// TODO(roasbeef): node ann needs to wait on existing channel updates
		return
	case *channeldb.LightningNode:
		return
	case *lnwire.AnnounceSignatures:
		// TODO(roasbeef): need to wait on chan ann?
		return
	}
}

// CompleteJob returns a free slot to the set of available job slots. This
// should be called once a job has been fully completed. Otherwise, slots may
// not be returned to the internal scheduling, causing a deadlock when a new
// overflow job is attempted.
func (v *ValidationBarrier) CompleteJob() {
	select {
	case v.validationSemaphore <- struct{}{}:
	case <-v.quit:
	}
}

// WaitForDependants will block until any jobs that this job dependants on have
// finished executing. This allows us a graceful way to schedule goroutines
// based on any pending uncompleted dependent jobs. If this job doesn't have an
// active dependent, then this function will return immediately.
func (v *ValidationBarrier) WaitForDependants(job interface{}) error {

	var (
		signal chan struct{}
		ok     bool
	)

	v.Lock()
	switch msg := job.(type) {

	// Any ChannelUpdate or NodeAnnouncement jobs will need to wait on the
	// completion of any active ChannelAnnouncement jobs related to them.
	case *channeldb.ChannelEdgePolicy:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		signal, ok = v.chanEdgeDependencies[shortID]
	case *channeldb.LightningNode:
		vertex := route.Vertex(msg.PubKeyBytes)
		signal, ok = v.nodeAnnDependencies[vertex]
	case *lnwire.ChannelUpdate:
		signal, ok = v.chanEdgeDependencies[msg.ShortChannelID]
	case *lnwire.NodeAnnouncement:
		vertex := route.Vertex(msg.NodeID)
		signal, ok = v.nodeAnnDependencies[vertex]

	// Other types of jobs can be executed immediately, so we'll just
	// return directly.
	case *lnwire.AnnounceSignatures:
		// TODO(roasbeef): need to wait on chan ann?
		v.Unlock()
		return nil
	case *channeldb.ChannelEdgeInfo:
		v.Unlock()
		return nil
	case *lnwire.ChannelAnnouncement:
		v.Unlock()
		return nil
	}
	v.Unlock()

	// If we do have an active job, then we'll wait until either the signal
	// is closed, or the set of jobs exits.
	if ok {
		select {
		case <-v.quit:
			return ErrVBarrierShuttingDown
		case <-signal:
			return nil
		}
	}

	return nil
}

// SignalDependants will signal any jobs that are dependent on this job that
// they can continue execution. If the job doesn't have any dependants, then
// this function sill exit immediately.
func (v *ValidationBarrier) SignalDependants(job interface{}) {
	v.Lock()
	defer v.Unlock()

	switch msg := job.(type) {

	// If we've just finished executing a ChannelAnnouncement, then we'll
	// close out the signal, and remove the signal from the map of active
	// ones. This will allow any dependent jobs to continue execution.
	case *channeldb.ChannelEdgeInfo:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		finSignal, ok := v.chanAnnFinSignal[shortID]
		if ok {
			close(finSignal)
			delete(v.chanAnnFinSignal, shortID)
		}
	case *lnwire.ChannelAnnouncement:
		finSignal, ok := v.chanAnnFinSignal[msg.ShortChannelID]
		if ok {
			close(finSignal)
			delete(v.chanAnnFinSignal, msg.ShortChannelID)
		}

		delete(v.chanEdgeDependencies, msg.ShortChannelID)

	// For all other job types, we'll delete the tracking entries from the
	// map, as if we reach this point, then all dependants have already
	// finished executing and we can proceed.
	case *channeldb.LightningNode:
		delete(v.nodeAnnDependencies, route.Vertex(msg.PubKeyBytes))
	case *lnwire.NodeAnnouncement:
		delete(v.nodeAnnDependencies, route.Vertex(msg.NodeID))
	case *lnwire.ChannelUpdate:
		delete(v.chanEdgeDependencies, msg.ShortChannelID)
	case *channeldb.ChannelEdgePolicy:
		shortID := lnwire.NewShortChanIDFromInt(msg.ChannelID)
		delete(v.chanEdgeDependencies, shortID)

	case *lnwire.AnnounceSignatures:
		return
	}
}
