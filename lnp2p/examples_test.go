package lnp2p_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/lightningnetwork/lnd/actor"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/msgmux"
	"github.com/lightningnetwork/lnd-nu-peer-api/lnp2p"
)

// Example_simplePeerBasic demonstrates basic SimplePeer usage.
func Example_simplePeerBasic() {
	// Parse target node address.
	target, err := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")
	if err != nil {
		log.Fatal(err)
	}

	// Create peer configuration.
	cfg := lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	}

	// Create and connect lnp2p.
	p, err := lnp2p.NewSimplePeer(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer p.Close()

	ctx := context.Background()
	if err := p.Connect(ctx); err != nil {
		log.Fatal(err)
	}

	// Send a ping message.
	ping := lnwire.NewPing(100)
	if err := p.SendMessage(ping); err != nil {
		log.Fatal(err)
	}

	// Process incoming messages.
	go func() {
		for msg := range p.ReceiveMessages() {
			fmt.Printf("Received: %T\n", msg)
		}
	}()
}

// Example_simplePeerWithIterator demonstrates using the message iterator pattern.
func Example_simplePeerWithIterator() {
	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")

	cfg := lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	}

	p, _ := lnp2p.NewSimplePeer(cfg)
	defer p.Close()

	ctx := context.Background()
	p.Connect(ctx)

	// Use iterator to process messages.
	for msg := range p.ReceiveMessages() {
		switch m := msg.(type) {
		case lnwire.ChannelUpdate:
			fmt.Printf("Channel update for %v\n", m.SCID())
		case *lnwire.NodeAnnouncement:
			fmt.Printf("Node announcement\n")
		case *lnwire.Error:
			fmt.Printf("Error: %s\n", m.Data)
			return
		}
	}
}

// Example_simplePeerWithMsgMux demonstrates msgmux integration.
func Example_simplePeerWithMsgMux() {
	// Create message router.
	router := msgmux.NewMultiMsgRouter()
	
	// Register endpoints for different message types.
	pingEndpoint := &pingHandler{}
	router.RegisterEndpoint(pingEndpoint)

	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")

	// Create peer with msgmux router.
	cfg := lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
		MsgRouter:    fn.Some[msgmux.Router](router),
	}

	p, _ := lnp2p.NewSimplePeer(cfg)
	defer p.Close()

	ctx := context.Background()
	p.Connect(ctx)

	// Enable broadcast mode to send messages to both iterator and router.
	p.EnableBroadcastMode(router)

	// Messages will be automatically routed to registered endpoints.
	time.Sleep(10 * time.Second)
}

// pingHandler is an example msgmux endpoint.
type pingHandler struct{}

func (h *pingHandler) Name() msgmux.EndpointName {
	return "ping-handler"
}

func (h *pingHandler) CanHandle(msg msgmux.PeerMsg) bool {
	_, ok := msg.Message.(*lnwire.Ping)
	return ok
}

func (h *pingHandler) SendMessage(ctx context.Context, msg msgmux.PeerMsg) bool {
	if ping, ok := msg.Message.(*lnwire.Ping); ok {
		fmt.Printf("Received ping requesting %d bytes\n", ping.NumPongBytes)
		return true
	}
	return false
}

// Example_peerActor demonstrates the actor-based peer pattern.
func Example_peerActor() {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create service key for message distribution.
	messageKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("message-handler")

	// Create message handler actor.
	messageHandler := &messageHandlerActor{}
	handlerBehavior := actor.NewFunctionBehavior(messageHandler.Receive)
	
	// Register handler with system.
	actor.RegisterWithSystem(system, "msg-handler", messageKey, handlerBehavior)

	// Create peer connection.
	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")
	conn, _ := lnp2p.NewSimplePeer(lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	})

	// Create peer actor configuration.
	peerCfg := lnp2p.PeerActorConfig{
		Connection:   conn,
		MessageSinks: []*lnp2p.MessageSink{{ServiceKey: messageKey}},
		Receptionist: system.Receptionist(),
		AutoConnect:  true,
	}

	// Create and register peer actor.
	peerRef, err := lnp2p.CreatePeerService(system, "test-peer", peerCfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// Request connection.
	future := peerRef.Ask(ctx, &lnp2p.ConnectRequest{
		Timeout: 30 * time.Second,
	})

	// Wait for connection.
	result := future.Await(ctx)
	if connResp, err := result.Unpack(); err == nil {
		if resp, ok := connResp.(*lnp2p.ConnectResponse); ok && resp.Success {
			fmt.Println("Connected successfully")
		}
	}

	// Send a message.
	sendFuture := peerRef.Ask(ctx, &lnp2p.SendMessageRequest{
		Message: lnwire.NewPing(100),
	})

	sendResult := sendFuture.Await(ctx)
	if _, err := sendResult.Unpack(); err != nil {
		log.Printf("Failed to send: %v", err)
	}
}

// messageHandlerActor handles messages received from peers.
type messageHandlerActor struct{}

func (a *messageHandlerActor) Receive(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
	switch m := msg.(type) {
	case *lnp2p.MessageReceived:
		fmt.Printf("Received %T from %x\n", m.Message, m.From.SerializeCompressed())
		
		// Send acknowledgment, potentially with a response.
		return fn.Ok[lnp2p.PeerResponse](&lnp2p.MessageReceivedAck{
			Processed: true,
		})

	case *lnp2p.ConnectionStateChange:
		fmt.Printf("Connection state changed to: %s\n", m.State)
		return fn.Ok[lnp2p.PeerResponse](&lnp2p.ConnectionStateAck{})

	case *lnp2p.PeerError:
		fmt.Printf("Peer error: %v\n", m.Error)
		return fn.Ok[lnp2p.PeerResponse](&lnp2p.PeerErrorAck{
			ShouldDisconnect: m.ErrorType == lnp2p.ErrorTypeProtocol,
		})

	default:
		return fn.Ok[lnp2p.PeerResponse](nil)
	}
}

// Example_connectionEvents demonstrates monitoring connection lifecycle.
func Example_connectionEvents() {
	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")

	cfg := lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	}

	p, _ := lnp2p.NewSimplePeer(cfg)
	defer p.Close()

	// Monitor connection events.
	go func() {
		for event := range p.ConnectionEvents() {
			fmt.Printf("[%s] State: %s", event.Timestamp.Format("15:04:05"), event.State)
			if event.Error != nil {
				fmt.Printf(" - Error: %v", event.Error)
			}
			if event.Details != "" {
				fmt.Printf(" - %s", event.Details)
			}
			fmt.Println()
		}
	}()

	ctx := context.Background()
	p.Connect(ctx)

	// Keep running for demonstration.
	time.Sleep(5 * time.Second)
}


// Example_parallelActors demonstrates parallel message processing with actors.
func Example_parallelActors() {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create multiple service keys for different concerns.
	gossipKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("gossip")
	channelKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("channel")
	routingKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("routing")

	// Register specialized actors for each concern.
	registerSpecializedActors(system, gossipKey, channelKey, routingKey)

	// Create peer connection.
	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")
	conn, _ := lnp2p.NewSimplePeer(lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	})

	// Create peer actor that distributes to all service keys.
	peerCfg := lnp2p.PeerActorConfig{
		Connection: conn,
		MessageSinks: []*lnp2p.MessageSink{
			{ServiceKey: gossipKey},
			{ServiceKey: channelKey},
			{ServiceKey: routingKey},
		},
		Receptionist: system.Receptionist(),
		AutoConnect:  true,
	}

	// Create peer service.
	_, err := lnp2p.CreatePeerService(system, "multi-peer", peerCfg)
	if err != nil {
		log.Fatal(err)
	}

	// Messages will be distributed to all registered actors in parallel.
	fmt.Println("Peer actor created with parallel message distribution")
}

func registerSpecializedActors(system *actor.ActorSystem,
	gossipKey, channelKey, routingKey actor.ServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]) {
	
	// Gossip handler.
	gossipBehavior := actor.NewFunctionBehavior(func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
		if m, ok := msg.(*lnp2p.MessageReceived); ok {
			switch m.Message.(type) {
			case *lnwire.NodeAnnouncement, lnwire.ChannelAnnouncement:
				fmt.Println("Gossip actor processing announcement")
			}
		}
		return fn.Ok[lnp2p.PeerResponse](nil)
	})
	actor.RegisterWithSystem(system, "gossip-actor", gossipKey, gossipBehavior)

	// Channel handler.
	channelBehavior := actor.NewFunctionBehavior(func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
		if m, ok := msg.(*lnp2p.MessageReceived); ok {
			switch m.Message.(type) {
			case *lnwire.OpenChannel, *lnwire.AcceptChannel:
				fmt.Println("Channel actor processing channel message")
			}
		}
		return fn.Ok[lnp2p.PeerResponse](nil)
	})
	actor.RegisterWithSystem(system, "channel-actor", channelKey, channelBehavior)

	// Routing handler.
	routingBehavior := actor.NewFunctionBehavior(func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
		if m, ok := msg.(*lnp2p.MessageReceived); ok {
			switch m.Message.(type) {
			case *lnwire.UpdateAddHTLC, *lnwire.UpdateFulfillHTLC:
				fmt.Println("Routing actor processing HTLC")
			}
		}
		return fn.Ok[lnp2p.PeerResponse](nil)
	})
	actor.RegisterWithSystem(system, "routing-actor", routingKey, routingBehavior)
}

// Example_dynamicServiceKeys demonstrates dynamically managing service keys.
func Example_dynamicServiceKeys() {
	// Create actor system.
	system := actor.NewActorSystem()
	defer system.Shutdown()

	// Create peer connection.
	target, _ := lnp2p.ParseNodeAddress("0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798@localhost:9735")
	conn, _ := lnp2p.NewSimplePeer(lnp2p.SimplePeerConfig{
		KeyGenerator: &lnp2p.EphemeralKeyGenerator{},
		Target:       *target,
		Features:     lnp2p.DefaultTestFeatures(),
		Timeouts:     fn.Some(lnp2p.DefaultTimeouts()),
	})

	// Start with no service keys.
	peerCfg := lnp2p.PeerActorConfig{
		Connection:   conn,
		MessageSinks: []*lnp2p.MessageSink{},
		Receptionist: system.Receptionist(),
		AutoConnect:  false,
	}

	// Create peer service.
	peerRef, err := lnp2p.CreatePeerService(system, "dynamic-peer", peerCfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()

	// Initially no keys registered.
	keysFuture := peerRef.Ask(ctx, &lnp2p.GetServiceKeysRequest{})
	result := keysFuture.Await(ctx)
	if resp, err := result.Unpack(); err == nil {
		if keysResp, ok := resp.(*lnp2p.GetServiceKeysResponse); ok {
			fmt.Printf("Initial keys: %d\n", len(keysResp.ServiceKeys))
		}
	}

	// Create and register a new handler.
	loggingKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("logging")
	loggingBehavior := actor.NewFunctionBehavior(func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
		if m, ok := msg.(*lnp2p.MessageReceived); ok {
			fmt.Printf("Logging: received %T\n", m.Message)
		}
		return fn.Ok[lnp2p.PeerResponse](nil)
	})
	actor.RegisterWithSystem(system, "logger", loggingKey, loggingBehavior)

	// Add the service key to the lnp2p.
	addFuture := peerRef.Ask(ctx, &lnp2p.AddServiceKeyRequest{
		MessageSink: &lnp2p.MessageSink{
			ServiceKey: loggingKey,
		},
	})
	addResult := addFuture.Await(ctx)
	if resp, err := addResult.Unpack(); err == nil {
		if addResp, ok := resp.(*lnp2p.AddServiceKeyResponse); ok {
			fmt.Printf("Added key, success: %v, total keys: %d\n", 
				addResp.Success, addResp.CurrentKeyCount)
		}
	}

	// Later, add another handler.
	metricsKey := actor.NewServiceKey[lnp2p.PeerMessage, lnp2p.PeerResponse]("metrics")
	metricsBehavior := actor.NewFunctionBehavior(func(ctx context.Context, msg lnp2p.PeerMessage) fn.Result[lnp2p.PeerResponse] {
		if _, ok := msg.(*lnp2p.MessageReceived); ok {
			// Update metrics counter.
			fmt.Println("Metrics: message received")
		}
		return fn.Ok[lnp2p.PeerResponse](nil)
	})
	actor.RegisterWithSystem(system, "metrics", metricsKey, metricsBehavior)

	// Add metrics key.
	addFuture2 := peerRef.Ask(ctx, &lnp2p.AddServiceKeyRequest{
		MessageSink: &lnp2p.MessageSink{
			ServiceKey: metricsKey,
		},
	})
	addResult2 := addFuture2.Await(ctx)
	if resp, err := addResult2.Unpack(); err == nil {
		if addResp, ok := resp.(*lnp2p.AddServiceKeyResponse); ok {
			fmt.Printf("Added metrics key, total keys: %d\n", addResp.CurrentKeyCount)
		}
	}

	// Remove the logging key.
	removeFuture := peerRef.Ask(ctx, &lnp2p.RemoveServiceKeyRequest{
		ServiceKey: loggingKey,
	})
	removeResult := removeFuture.Await(ctx)
	if resp, err := removeResult.Unpack(); err == nil {
		if removeResp, ok := resp.(*lnp2p.RemoveServiceKeyResponse); ok {
			fmt.Printf("Removed key, success: %v, remaining keys: %d\n",
				removeResp.Success, removeResp.CurrentKeyCount)
		}
	}

	// Messages will now only go to the metrics handler.
	fmt.Println("Dynamic service key management complete")
}