# Lightning Network P2P Package

The `lnp2p` package provides a standalone P2P layer for the Lightning Network that can be used independently of lnd's internal subsystems. It abstracts away the complexity of brontide encrypted connections (Lightning's Noise protocol implementation) and offers multiple abstraction levels for different use cases - from simple message exchange in tests to concurrent message processing in production systems.

## Core Abstractions

### P2PConnection Interface

The foundation of the package is a simple interface that any P2P connection must implement:

```go
type P2PConnection interface {
    Connect(ctx context.Context) error
    SendMessage(msg lnwire.Message) error
    ReceiveMessages() iter.Seq[lnwire.Message]
    ConnectionEvents() iter.Seq[ConnectionEvent]
    RemotePubKey() *btcec.PublicKey
    LocalPubKey() *btcec.PublicKey
    IsConnected() bool
    Close() error
}
```

The interface uses Go's iterator pattern for message and event streaming, allowing flexible consumption patterns without prescribing a specific concurrency model.

## SimplePeer: Basic P2P Operations

SimplePeer provides a straightforward implementation of the P2PConnection interface. It handles all the complexity of establishing brontide encrypted connections (Noise protocol handshake), message framing, and the Lightning Network init message exchange - you just need to provide a key and target address.

### Quick Start

```go
// Parse target node address.
target, err := lnp2p.ParseNodeAddress("03abc...def@node.example.com:9735")
if err != nil {
    return err
}

// Create peer with minimal configuration.
cfg := lnp2p.SimplePeerConfig{
    KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
    Target:       *target,
    Features:     lnp2p.DefaultFeatures(),
    Timeouts:     lnp2p.DefaultTimeouts(),
}

peer, err := lnp2p.NewSimplePeer(cfg)
if err != nil {
    return err
}
defer peer.Close()

// Connect and start exchanging messages.
if err := peer.Connect(context.Background()); err != nil {
    return err
}
```

### Message Exchange

```go
// Receive messages using iterator pattern.
go func() {
    for msg := range peer.ReceiveMessages() {
        switch m := msg.(type) {
        case *lnwire.ChannelUpdate:
            // Process channel update.
            processUpdate(m)
        case *lnwire.Error:
            log.Printf("Peer error: %s", m.Data)
        case *lnwire.Ping:
            // Pings are handled automatically.
        }
    }
}()

// Send messages.
query := &lnwire.QueryChannelRange{
    ChainHash:        mainnetGenesis,
    FirstBlockHeight: 800000,
    NumBlocks:        1000,
}
peer.SendMessage(query)
```

### Connection Events

```go
// Monitor connection lifecycle.
for event := range peer.ConnectionEvents() {
    log.Printf("[%s] State: %s", event.Timestamp, event.State)

    if event.State == lnp2p.StateConnected {
        // Connection established.
    } else if event.State == lnp2p.StateDisconnected && event.Error != nil {
        // Handle disconnection.
    }
}
```

## PeerActor: Concurrent Message Processing

PeerActor builds on SimplePeer to provide concurrent message processing using the actor model. Different subsystems can register as MessageSinks to receive and process relevant messages independently.

### MessageSinks

```go
// MessageSink routes messages to specific handlers with optional filtering.
type MessageSink struct {
    ServiceKey actor.ServiceKey[PeerMessage, PeerResponse]
    Filter     MessageFilter  // func(lnwire.Message) bool
}
```

### Example: Concurrent Message Handling

```go
// Create actor system for concurrent processing.
system := actor.NewActorSystem()
defer system.Shutdown()

// Define a handler for channel messages.
channelKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse](
    "channel-handler",
)

// Register handler behavior.
behavior := actor.NewFunctionBehavior(func(
    ctx context.Context,
    msg lnp2p.PeerMessage,
) fn.Result[lnp2p.PeerResponse] {
    if m, ok := msg.(*lnp2p.MessageReceived); ok {
        if update, ok := m.Message.(*lnwire.ChannelUpdate); ok {
            // Process channel update.
            processChannelUpdate(update)
            return fn.Ok[lnp2p.PeerResponse](
                &lnp2p.MessageReceivedAck{Processed: true},
            )
        }
    }
    return fn.Ok[lnp2p.PeerResponse](nil)
})

actor.RegisterWithSystem(system, "channel-handler", channelKey, behavior)

// Create filter for channel messages.
channelFilter := func(msg lnwire.Message) bool {
    switch msg.(type) {
    case *lnwire.ChannelUpdate, *lnwire.ChannelAnnouncement:
        return true
    default:
        return false
    }
}
```

### Wiring It Together

```go
// Create SimplePeer for underlying connection.
simplePeer, err := createSimplePeer()
if err != nil {
    return err
}

// Configure PeerActor with message sinks.
cfg := lnp2p.PeerActorConfig{
    Connection:   simplePeer,
    Receptionist: system.Receptionist(),
    MessageSinks: []*lnp2p.MessageSink{
        {
            ServiceKey: channelKey,
            Filter:     channelFilter,
        },
    },
    AutoConnect: true,
}

// Create and start the PeerActor.
peerActor, err := lnp2p.NewPeerActor(cfg)
if err != nil {
    return err
}

startResult := peerActor.Start(ctx)
if resp, err := startResult.Unpack(); err != nil || !resp.Success {
    return err
}
```

With this setup, the PeerActor automatically connects and distributes incoming messages to all registered handlers based on their filters. Multiple subsystems can process messages concurrently without manual synchronization.

## Testing

The package includes a comprehensive test harness that simplifies testing P2P logic:

```go
func TestMyProtocol(t *testing.T) {
    // Create test harness with mock connection.
    harness := newTestHarness(t, &MockConnectionConfig{
        IsConnected:  true,
        RemotePubKey: newTestPubKey(),
        LocalPubKey:  newTestPubKey(),
        Messages: []lnwire.Message{
            &lnwire.Ping{NumPongBytes: 8},
            &lnwire.ChannelUpdate{
                ShortChannelID: lnwire.NewShortChanIDFromInt(12345),
            },
        },
    }).withActorSystem()
    defer harness.cleanupAll()

    // Register test handler.
    handlerKey := harness.createServiceKey("test-handler",
        func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
            // Test logic here.
            return fn.Ok[lnp2p.PeerResponse](
                &lnp2p.MessageReceivedAck{Processed: true},
            )
        })

    // Create and start PeerActor.
    peerActor := harness.createPeerActor(handlerKey)
    harness.startPeerActor(peerActor)

    // Assertions...
}
```

## Design Principles

- **Layered Abstraction**: Multiple levels from simple to sophisticated use cases
- **Composability**: Mix and match patterns (iterator, actor, msgmux)
- **Testability**: Comprehensive mocking and test harness
- **Decoupling**: Minimal dependencies on lnd internals
- **Explicit State**: Observable connection lifecycle and message routing

## Getting Started

```bash
go get github.com/lightningnetwork/lnd-nu-peer-api/lnp2p
```

Start with SimplePeer for basic use cases. As your needs grow, transition to PeerActor for concurrent message processing. See `examples_test.go` for working code examples.

## Use Cases

- **Testing**: Write unit tests without lnd's full infrastructure
- **Custom Overlays**: Build stress testing tools and network overlays with minimal boilerplate
- **Protocol Development**: Experiment with new message types and handshakes
- **Analysis Tools**: Build protocol analyzers and network monitors
- **Simulations**: Model network conditions and failure scenarios

## Architecture

```
Application Layer (custom overlays, tests, etc.)
         │
         ▼
  PeerActor Layer (concurrent processing)
         │
         ▼
   SimplePeer Layer (basic operations)
         │
         ▼
  P2PConnection Interface (abstraction)
         │
         ▼
   Transport Layer (brontide, noise)
```