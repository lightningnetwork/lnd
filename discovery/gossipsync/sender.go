package gossipsync

import (
	"context"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/time/rate"
)

const (
	// assumedMsgSize is the size charged to the rate limiters for a
	// message whose serialized size we can't compute.
	assumedMsgSize = 1024
)

// PeerConn is the part of a peer connection the syncer uses.
type PeerConn interface {
	// PubKey returns the peer's public key.
	PubKey() [33]byte

	// SendMessageLazy sends messages to the peer at low priority. If sync
	// is set, it blocks until the messages are written.
	SendMessageLazy(sync bool, msgs ...lnwire.Message) error
}

// sender sends gossip to one peer, charging every message to that peer's
// rate limiter and to the rate limiter shared by all peers.
type sender struct {
	conn PeerConn

	// peer limits the bytes per second sent to this peer.
	peer *rate.Limiter

	// global limits the bytes per second sent to all peers.
	global *rate.Limiter
}

// send sends msgs in order, waiting on both rate limiters before each one.
func (s *sender) send(ctx context.Context, sync bool,
	msgs ...lnwire.Message) error {

	for _, msg := range msgs {
		size := msgSize(msg)
		if err := wait(ctx, s.peer, size); err != nil {
			return err
		}
		if err := wait(ctx, s.global, size); err != nil {
			return err
		}

		if err := s.conn.SendMessageLazy(sync, msg); err != nil {
			return err
		}
	}

	return nil
}

// msgSize returns the size a message is charged to the rate limiters.
func msgSize(msg lnwire.Message) int {
	sized, ok := msg.(lnwire.SizeableMessage)
	if !ok {
		return assumedMsgSize
	}

	size, err := sized.SerializedSize()
	if err != nil {
		return assumedMsgSize
	}

	return int(size)
}

// wait blocks until the limiter allows n more bytes, or ctx is done. A nil
// limiter never waits.
func wait(ctx context.Context, rl *rate.Limiter, n int) error {
	if rl == nil {
		return nil
	}

	// A reservation larger than the burst can never be satisfied. The
	// config requires every burst to fit the largest wire message, so
	// this only happens on a misconfiguration, which we let through
	// rather than block forever.
	r := rl.ReserveN(time.Now(), n)
	if !r.OK() {
		return nil
	}

	delay := r.Delay()
	if delay == 0 {
		return nil
	}

	t := time.NewTimer(delay)
	defer t.Stop()

	select {
	case <-t.C:
		return nil

	case <-ctx.Done():
		r.Cancel()

		return ctx.Err()
	}
}
