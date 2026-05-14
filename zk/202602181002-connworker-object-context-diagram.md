# Connection Worker Object Context

Structural overview of how the server communicates with per-peer connection
workers and what each caller needs from the worker layer.

```mermaid
flowchart LR
    subgraph triggers["Trigger Events"]
        startup["Startup"]
        gossip["Gossip addr update"]
        disconnect["Peer disconnects"]
        user["User: ConnectToPeer"]
        inbound["Inbound connection"]
        prune["Channel close / ban"]
    end

    subgraph helpers["Server Helpers"]
        create["getOrCreateWorker"]
        send["sendWorkerCmd"]
        stop["stopWorker"]
    end

    subgraph worker["connWorker goroutine"]
        cmdChan["cmdChan"]
        loop["Run / dialLoop"]
        dial["DialContext"]
    end

    startup -->|cmdConnect| create
    gossip -->|cmdUpdateAddrs| send
    disconnect -->|cmdConnect + backoff| send
    user -->|cmdConnect| create
    inbound -->|cmdStandDown| send
    prune -->|cmdStop| stop

    create --> cmdChan
    send --> cmdChan
    stop --> cmdChan

    cmdChan --> loop --> dial
    dial -->|success| cb["OutboundPeerConnected"]
```

Tags: #diagram #architecture #persistent-connections

## References
- Visualizes the trigger events and commands handled by:
  [[202602181001-connworker-run-loop.md]]
