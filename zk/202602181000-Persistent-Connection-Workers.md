# Persistent Connection Workers

LND maintains persistent connections to peers it shares channels with. The
server must track which peers are persistent, manage reconnection backoff, store
known addresses, and coordinate dial attempts — all while responding to inbound
connections, gossip address updates, user-initiated connects, and channel
lifecycle events.

The original design spread this state across five loosely coupled maps
(`persistentPeers`, `persistentPeersBackoff`, `persistentPeerAddrs`,
`persistentConnReqs`, `persistentRetryCancels`) and delegated dialing to the
connection manager. Multiple server methods mutated these maps from different
goroutines, creating race windows where connection requests could accumulate
without bound.

The worker architecture consolidates the entire dial/retry/backoff lifecycle for
each persistent peer into a single [[202602181001-connworker-run-loop.md]]
goroutine that the server communicates with via a typed command channel. A
worker's existence in `persistentWorkers` is the sole indicator that a peer is
persistent; its removal means the peer was pruned or disconnected.

The server interacts with workers through three helpers: `getOrCreateWorker`
(creates or returns the existing worker), `stopWorker` (sends stop and removes
from map), and `sendWorkerCmd` (delivers a command to an existing worker). The
command vocabulary is small: connect, update addresses, stand down, and stop.

The structural relationships between these objects are visualized in
[[202602181002-connworker-object-context-diagram.md]]. The runtime execution
flow with error paths is shown in
[[202602181003-connworker-execution-flow-diagram.md]].

Tags: #architecture #persistent-connections #peer-management

## References
- Execution model: [[202602181001-connworker-run-loop.md]]
- Structural overview: [[202602181002-connworker-object-context-diagram.md]]
- Runtime flow: [[202602181003-connworker-execution-flow-diagram.md]]
