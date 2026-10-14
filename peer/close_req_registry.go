package peer

import (
	"sync"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwire"
)

// closeReqRegistry tracks the callers waiting on a cooperative close for a
// channel. A channel can have more than one caller while an earlier one is
// still live, because a fee bump for a close already in progress registers
// another, so callers are held as a set rather than as a single record.
//
// A caller is resolved by taking its record out of the set, and taking is the
// only way to resolve one, so a caller is resolved at most once however many
// paths race to end the close. A path that acts for one caller takes that
// caller and no other; a path that ends a close for the whole channel takes
// whichever caller is left, which is what answers a caller whose closer was
// retired underneath it.
type closeReqRegistry struct {
	mu   sync.Mutex
	reqs map[lnwire.ChannelID][]*htlcswitch.ChanClose
}

// newCloseReqRegistry returns an empty registry.
func newCloseReqRegistry() *closeReqRegistry {
	return &closeReqRegistry{
		reqs: make(map[lnwire.ChannelID][]*htlcswitch.ChanClose),
	}
}

// add records a caller waiting on a close for the channel.
func (r *closeReqRegistry) add(chanID lnwire.ChannelID,
	req *htlcswitch.ChanClose) {

	r.mu.Lock()
	defer r.mu.Unlock()

	r.reqs[chanID] = append(r.reqs[chanID], req)
}

// takeIf removes and returns the given caller when it is still recorded for
// the channel, and nil otherwise. This is how a path acting for one caller
// takes that caller and cannot take another's, so that two paths which act for
// different callers on one channel cannot both take the same one.
func (r *closeReqRegistry) takeIf(chanID lnwire.ChannelID,
	want *htlcswitch.ChanClose) *htlcswitch.ChanClose {

	if want == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	for i, held := range r.reqs[chanID] {
		if held != want {
			continue
		}

		return r.removeLocked(chanID, i)
	}

	return nil
}

// takeAll removes and returns every caller recorded for the channel. A path
// that ends a close for the channel as a whole answers all of them, since
// each one is owed the same outcome.
func (r *closeReqRegistry) takeAll(
	chanID lnwire.ChannelID) []*htlcswitch.ChanClose {

	r.mu.Lock()
	defer r.mu.Unlock()

	reqs := r.reqs[chanID]
	delete(r.reqs, chanID)

	return reqs
}

// peek returns a caller recorded for the channel without taking it.
func (r *closeReqRegistry) peek(
	chanID lnwire.ChannelID) *htlcswitch.ChanClose {

	r.mu.Lock()
	defer r.mu.Unlock()

	reqs := r.reqs[chanID]
	if len(reqs) == 0 {
		return nil
	}

	return reqs[0]
}

// removeLocked drops and returns the caller at index i, which must be a valid
// index of the channel's callers. Its only caller finds i by ranging over that
// same slice while holding the lock, so there is nothing to check here.
func (r *closeReqRegistry) removeLocked(chanID lnwire.ChannelID,
	i int) *htlcswitch.ChanClose {

	reqs := r.reqs[chanID]
	taken := reqs[i]
	reqs = append(reqs[:i], reqs[i+1:]...)

	if len(reqs) == 0 {
		delete(r.reqs, chanID)
	} else {
		r.reqs[chanID] = reqs
	}

	return taken
}
