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
	// meaningful for cmdConnect. A zero value means dial immediately and
	// resets any prior exponential backoff — this is intentional for fresh
	// connect requests (e.g. ConnectToPeer, startup).
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

// newConnWorker creates a connection worker for the given persistent peer. The
// worker is idle until it receives a command on cmdChan. The caller must start
// the worker via go w.Run().
func newConnWorker(pubKeyStr string, perm bool, cfg connWorkerCfg) *connWorker {
	return &connWorker{
		pubKeyStr: pubKeyStr,
		perm:      perm,
		cmdChan:   make(chan connWorkerMsg, 1),
		backoff:   cfg.minBackoff,
		cfg:       cfg,
	}
}

// Run is the main event loop for the connection worker. It waits for commands
// on cmdChan and dispatches them. cmdConnect starts a dial loop; cmdStandDown
// cancels an active dial and returns to idle; cmdStop exits the goroutine. Run
// returns when cmdStop is received or the server's quit channel is closed.
//
// NOTE: This method MUST be run as a goroutine.
func (w *connWorker) Run() {
	for {
		select {
		case msg := <-w.cmdChan:
			switch msg.cmd {
			case cmdConnect:
				w.addrs = msg.addrs
				w.backoff = msg.backoff
				if !w.dialLoop() {
					return
				}

			case cmdUpdateAddrs:
				w.addrs = msg.addrs

			case cmdStandDown:
				// Already idle, nothing to cancel.

			case cmdStop:
				return
			}

		case <-w.cfg.quit:
			return
		}
	}
}

// dialLoop runs successive dial rounds until a connection succeeds, a
// preempting command arrives, or the worker is stopped. Before the first round
// (and between subsequent rounds on failure), the worker waits for the current
// backoff duration. Returns true if the caller should continue the run loop
// (idle), false if the worker should exit (false) or be idled (true).
func (w *connWorker) dialLoop() bool {
	for {
		// Wait for the backoff before dialing. A zero backoff skips the
		// wait entirely (used for immediate connects at startup).
		if w.backoff > 0 {
			timer := time.NewTimer(w.backoff)

			select {
			case msg := <-w.cmdChan:
				timer.Stop()

				switch msg.cmd {
				case cmdConnect:
					w.addrs = msg.addrs
					w.backoff = msg.backoff
					continue

				case cmdUpdateAddrs:
					w.addrs = msg.addrs

					// TODO: Should we reset the backoff
					// timer here? Currently we restart the
					// full wait with the same duration. If
					// the addresses changed, dialing sooner
					// may be preferable.
					continue

				case cmdStandDown:
					return true

				case cmdStop:
					return false
				}

			case <-timer.C:

			case <-w.cfg.quit:
				timer.Stop()

				return false
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		result := w.tryAllAddresses(ctx, cancel)
		cancel()

		switch result {
		case dialSuccess:
			return true

		case dialRestart:
			continue

		case dialStop:
			return false

		case dialStandDown:
			return true

		case dialFailed:
			// All addresses failed. Increase backoff for the next
			// round. If the current backoff is zero (first
			// attempt), start from minBackoff.
			if w.backoff < w.cfg.minBackoff {
				w.backoff = w.cfg.minBackoff
			} else {
				w.backoff = computeNextBackoff(
					w.backoff, w.cfg.maxBackoff,
				)
			}
		}
	}
}

// dialResult enumerates the outcomes of a dial round.
type dialResult uint8

const (
	// dialSuccess means a connection was established and delivered.
	dialSuccess dialResult = iota

	// dialFailed means all addresses were tried without success.
	dialFailed

	// dialRestart means a new cmdConnect or cmdUpdateAddrs arrived during
	// the round, and the caller should restart.
	dialRestart

	// dialStandDown means a cmdStandDown arrived and the worker should
	// return to idle.
	dialStandDown

	// dialStop means cmdStop or quit was received and the worker should
	// exit.
	dialStop
)

// dialOutcome carries the result of a single dial attempt back from the
// goroutine to the select loop.
type dialOutcome struct {
	conn net.Conn
	err  error
}

// tryAllAddresses iterates over the worker's addresses, dialing each one.
// Between addresses it waits for the stagger delay, checking for preempting
// commands. Each dial runs in a goroutine so that the worker can respond to
// commands and quit signals during in-progress dials. The provided cancel
// function is called when a preempting command aborts the round.
func (w *connWorker) tryAllAddresses(ctx context.Context,
	cancel context.CancelFunc) dialResult {

	for i, addr := range w.addrs {
		// Stagger between addresses (skip for the first one).
		if i > 0 {
			timer := time.NewTimer(w.cfg.staggerDelay)
			select {
			case msg := <-w.cmdChan:
				timer.Stop()
				cancel()

				return w.handleMidDial(msg)

			case <-timer.C:

			case <-w.cfg.quit:
				timer.Stop()
				cancel()

				return dialStop
			}
		}

		srvrLog.Debugf("Dialing persistent peer %x addr=%v",
			w.pubKeyStr, addr)

		// Run the dial in a goroutine so we can select on commands and
		// quit concurrently.
		resultCh := make(chan dialOutcome, 1)
		go func() {
			conn, err := w.cfg.dialContext(ctx, addr)
			resultCh <- dialOutcome{conn: conn, err: err}
		}()

		select {
		case outcome := <-resultCh:
			if outcome.err != nil {
				srvrLog.Debugf("Failed to dial %v for peer "+
					"%x: %v", addr, w.pubKeyStr,
					outcome.err)
				continue
			}

			// Dial succeeded. Deliver the connection.
			w.cfg.onConnection(outcome.conn, addr)

			return dialSuccess

		case msg := <-w.cmdChan:
			cancel()
			// Wait for the dial goroutine to finish so we don't
			// leak it.
			<-resultCh

			return w.handleMidDial(msg)

		case <-w.cfg.quit:
			cancel()
			<-resultCh

			return dialStop
		}
	}

	return dialFailed
}

// handleMidDial processes a command that arrived during a stagger wait or
// between dial attempts, returning the appropriate dialResult.
func (w *connWorker) handleMidDial(msg connWorkerMsg) dialResult {
	switch msg.cmd {
	case cmdConnect:
		w.addrs = msg.addrs
		w.backoff = msg.backoff

		return dialRestart

	case cmdUpdateAddrs:
		w.addrs = msg.addrs

		return dialRestart

	case cmdStandDown:
		return dialStandDown

	case cmdStop:
		return dialStop

	default:
		return dialFailed
	}
}
