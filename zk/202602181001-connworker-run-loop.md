# Connection Worker Run Loop

Each persistent peer gets a single `connWorker` goroutine that owns its
dial/retry/backoff lifecycle. The worker starts idle and transitions between two
states: idle (waiting for commands) and dialing (actively attempting
connections).

In the idle state, the worker blocks on its command channel. A `cmdConnect`
message transitions it into the dial loop, carrying the target addresses and an
initial backoff duration. `cmdUpdateAddrs` replaces the address list without
starting a dial. `cmdStandDown` is a no-op when idle. `cmdStop` and the server's
quit channel terminate the goroutine.

The dial loop first waits for the backoff duration (skipped when zero), then
iterates over addresses with a stagger delay between each. Every dial runs in a
child goroutine under a cancellable context — the
[[202602181004-brontide-dial-context.md]] makes the encrypted handshake itself
interruptible — so the worker can respond to preempting commands or shutdown
without leaking goroutines. When a command arrives mid-dial, the context is
canceled, the dial goroutine is drained, and the command is dispatched.

On dial failure across all addresses, the backoff increases via exponential
backoff with randomized jitter. On success, the connection is delivered through
the `onConnection` callback and the worker returns to idle.

The backoff is computed by the pure `peerBackoff` function which considers
connection stability: short-lived connections double the backoff, stable
connections (exceeding `stableConnDuration`) reduce it, and very stable
connections reset to `minBackoff`.

The buffered command channel (capacity 1) ensures the server never blocks when
sending commands under normal operation.

Tags: #architecture #persistent-connections #concurrency

## References
- Cancellable dial mechanism enabling mid-dial preemption:
  [[202602181004-brontide-dial-context.md]]
