package lnd

import (
	"context"
	"net"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// connWorkerCmd enumerates the commands that the server can send to a
// connection worker.
type connWorkerCmd uint8

const (
	// cmdConnect instructs the worker to begin dialing the provided
	// addresses after waiting for the specified backoff duration.
	cmdConnect connWorkerCmd = iota

	// cmdUpdateAddrs replaces the worker's address list. If a dial loop is
	// active, it is restarted with the new addresses.
	cmdUpdateAddrs

	// cmdStandDown cancels any in-progress dial and returns the worker to
	// an idle state. Used when an inbound connection makes the outbound
	// attempt unnecessary.
	cmdStandDown

	// cmdStop terminates the worker goroutine permanently.
	cmdStop
)

// connWorkerMsg carries a command and its associated data from the server to a
// connection worker.
type connWorkerMsg struct {
	// cmd is the command to execute.
	cmd connWorkerCmd

	// addrs is the list of addresses to dial, used by cmdConnect and
	// cmdUpdateAddrs.
	addrs []*lnwire.NetAddress

	// backoff is the duration to wait before the first dial attempt. Only
	// meaningful for cmdConnect.
	backoff time.Duration
}

// connWorkerCfg bundles the dependencies injected into a connection worker at
// creation time. All fields are set once and never modified.
type connWorkerCfg struct {
	// dialContext establishes a Brontide connection to the given address.
	// The context controls cancellation of the TCP dial and handshake.
	dialContext func(ctx context.Context,
		addr *lnwire.NetAddress) (net.Conn, error)

	// onConnection is called when a dial succeeds. The worker passes the
	// raw connection and the address that was dialed.
	onConnection func(conn net.Conn, addr *lnwire.NetAddress)

	// minBackoff is the floor for the exponential backoff.
	minBackoff time.Duration

	// maxBackoff is the ceiling for the exponential backoff.
	maxBackoff time.Duration

	// stableConnDuration is the minimum connection lifetime before backoff
	// reduction is applied.
	stableConnDuration time.Duration

	// staggerDelay is the pause between dialing successive addresses for
	// the same peer.
	staggerDelay time.Duration

	// quit is the server-wide shutdown signal.
	quit chan struct{}
}

// connWorker manages the dial/retry/backoff lifecycle for a single persistent
// peer. The server communicates with it exclusively through the cmdChan
// channel. A worker's existence in the server's persistentWorkers map means the
// peer is persistent; its removal means the peer has been disconnected or
// pruned.
type connWorker struct {
	// pubKeyStr identifies the remote peer (compressed pubkey as raw
	// string).
	pubKeyStr string

	// perm indicates whether the user explicitly requested this connection
	// via ConnectToPeer with perm=true. Non-perm workers are removed when
	// the peer's last channel closes.
	perm bool

	// cmdChan carries commands from the server to the worker's run loop.
	// Buffered with capacity 1 so the server never blocks under normal
	// operation.
	cmdChan chan connWorkerMsg

	// backoff is the current reconnection delay, maintained locally by the
	// worker across dial rounds.
	backoff time.Duration

	// addrs is the current set of addresses to dial.
	addrs []*lnwire.NetAddress

	// cfg holds the injected dependencies.
	cfg connWorkerCfg
}
