# Context-Cancellable Brontide Dial

Establishing a Brontide connection has two phases: a TCP connection and a
multi-round Noise protocol handshake. The TCP phase honours context cancellation
natively — the OS-level connect call propagates it through the injected
context-aware dialer.

The handshake phase is harder to interrupt. Its reads and writes are blocking
I/O with no built-in context support. The chosen approach treats connection
closure as a cancellation proxy: a watcher goroutine listens on the context's
done channel and closes the underlying TCP connection when signalled. This
immediately unblocks any in-progress handshake read or write, causing it to
return an error.

A completion signal prevents the watcher from closing a connection that finished
its handshake successfully. The caller signals this channel before returning the
live connection, neutralising the watcher before it can act.

This design enables the [[202602181001-connworker-run-loop.md]] to preempt
in-progress dials when commands arrive mid-handshake, without leaking goroutines
or leaving zombie connections open.

Tags: #architecture #networking #brontide #concurrency

## References
- Enables mid-dial preemption in: [[202602181001-connworker-run-loop.md]]
