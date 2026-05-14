# Connection Worker Execution Flow

Runtime flow of a single `connWorker` goroutine showing state transitions,
command handling at each stage, and error paths.

```mermaid
flowchart TD
    Start["Run() starts"] --> Idle["IDLE: select cmdChan or quit"]

    Idle -->|cmdConnect| SetState["Update addrs + backoff"]
    Idle -->|cmdUpdateAddrs| UpdateIdle["Update addrs only"]
    UpdateIdle --> Idle
    Idle -->|cmdStandDown| Idle
    Idle -->|cmdStop| Exit["EXIT"]
    Idle -->|quit| Exit

    SetState --> EnterLoop["dialLoop()"]

    EnterLoop --> CheckBackoff{"backoff > 0?"}
    CheckBackoff -->|no| StartDial["tryAllAddresses()"]
    CheckBackoff -->|yes| WaitTimer["Timer: select timer/cmd/quit"]

    WaitTimer -->|timer fires| StartDial
    WaitTimer -->|cmdConnect| SetState
    WaitTimer -->|cmdUpdateAddrs| EnterLoop
    WaitTimer -->|cmdStandDown| Idle
    WaitTimer -->|cmdStop or quit| Exit

    StartDial --> ForAddr["For each addr i=0..N"]

    ForAddr -->|"i > 0"| Stagger["Stagger delay: select timer/cmd/quit"]
    ForAddr -->|"i == 0"| SpawnDial["Spawn dial goroutine"]

    Stagger -->|timer fires| SpawnDial
    Stagger -->|cmd| CancelRound["cancel ctx, handleMidDial()"]
    Stagger -->|quit| DrainExit["cancel ctx"] --> Exit

    SpawnDial --> DialSelect["select: result / cmd / quit"]

    DialSelect -->|"err != nil"| NextAddr{"More addrs?"}
    DialSelect -->|"conn != nil"| Deliver["onConnection(conn)"] --> Idle
    DialSelect -->|cmd| CancelDrain["cancel ctx, drain goroutine, handleMidDial()"]
    DialSelect -->|quit| QuitDrain["cancel ctx, drain goroutine"] --> Exit

    NextAddr -->|yes| ForAddr
    NextAddr -->|no| BumpBackoff["Increase backoff"]
    BumpBackoff --> EnterLoop

    CancelRound -->|cmdConnect| SetState
    CancelRound -->|cmdUpdateAddrs| EnterLoop
    CancelRound -->|cmdStandDown| Idle
    CancelRound -->|cmdStop| Exit

    CancelDrain -->|cmdConnect| SetState
    CancelDrain -->|cmdUpdateAddrs| EnterLoop
    CancelDrain -->|cmdStandDown| Idle
    CancelDrain -->|cmdStop| Exit
```

Tags: #diagram #architecture #persistent-connections #concurrency

## References
- Visualizes runtime of: [[202602181001-connworker-run-loop.md]]
