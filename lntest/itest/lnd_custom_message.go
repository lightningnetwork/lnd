package itest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

// testCustomMessage tests sending and receiving of overridden custom message
// types (within the message type range usually reserved for protocol messages)
// via the send and subscribe custom message APIs.
func testCustomMessage(net *lntest.NetworkHarness, t *harnessTest) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	// At the end of our test, cancel our context and wait for all
	// goroutines to exit.
	defer func() {
		cancel()
		wg.Wait()
	}()

	var (
		overrideType1  uint32 = 554
		overrideType2  uint32 = 555
		msgOverrideArg        = "--protocol.custom-message=%v"
	)

	// Update Alice to accept custom protocol messages with type 1 but do
	// not allow Bob to handle them yet.
	net.Alice.Cfg.ExtraArgs = append(
		net.Alice.Cfg.ExtraArgs,
		fmt.Sprintf(msgOverrideArg, overrideType1),
	)
	require.NoError(t.t, net.RestartNode(net.Alice, nil, nil))

	// Wait for Alice's server to be active after the restart before we
	// try to subscribe to our message stream.
	require.NoError(t.t, net.Alice.WaitUntilServerActive())

	// Subscribe Alice to custom messages before we send any, so that we
	// don't miss any.
	msgClient, err := net.Alice.LightningClient.SubscribeCustomMessages(
		ctx, &lnrpc.SubscribeCustomMessagesRequest{},
	)
	require.NoError(t.t, err, "alice could not subscribe")

	// Create a channel to receive custom messages on.
	messages := make(chan *lnrpc.CustomMessage)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			// If we fail to receive, just exit. The test should
			// fail elsewhere if it doesn't get a message that it
			// was expecting.
			msg, err := msgClient.Recv()
			if err != nil {
				return
			}

			// Deliver the message into our channel or exit if the
			// test is shutting down.
			select {
			case messages <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	// Connect alice and bob so that they can exchange messages.
	net.EnsureConnected(t.t, net.Alice, net.Bob)

	// Create a custom message that is within our allowed range.
	msgType := uint32(lnwire.CustomTypeStart + 1)
	msgData := []byte{1, 2, 3}

	// Send it from Bob to Alice.
	ctxt, _ := context.WithTimeout(ctx, defaultTimeout)
	_, err = net.Bob.LightningClient.SendCustomMessage(
		ctxt, &lnrpc.SendCustomMessageRequest{
			Peer: net.Alice.PubKey[:],
			Type: msgType,
			Data: msgData,
		},
	)
	require.NoError(t.t, err, "bob could not send")

	// Wait for Alice to receive the message. It should come through because
	// it is within our allowed range.
	select {
	case msg := <-messages:
		// Check our type and data and (sanity) check the peer we got it
		// from.
		require.Equal(t.t, msgType, msg.Type, "first msg type wrong")
		require.Equal(t.t, msgData, msg.Data, "first msg data wrong")
		require.Equal(t.t, net.Bob.PubKey[:], msg.Peer, "first msg "+
			"peer wrong")

	case <-time.After(defaultTimeout):
		t.t.Fatalf("alice did not receive first custom message: %v",
			msgType)
	}

	// Try to send a message from Bob to Alice which has a message type
	// outside of the custom type range and assert that it fails.
	ctxt, _ = context.WithTimeout(ctx, defaultTimeout)
	_, err = net.Bob.LightningClient.SendCustomMessage(
		ctxt, &lnrpc.SendCustomMessageRequest{
			Peer: net.Alice.PubKey[:],
			Type: overrideType1,
			Data: msgData,
		},
	)
	require.Error(t.t, err, "bob should not be able to send type 1")

	// Now, restart Bob with the ability to send two different custom
	// protocol messages.
	net.Bob.Cfg.ExtraArgs = append(
		net.Bob.Cfg.ExtraArgs,
		fmt.Sprintf(msgOverrideArg, overrideType1),
		fmt.Sprintf(msgOverrideArg, overrideType2),
	)
	require.NoError(t.t, net.RestartNode(net.Bob, nil, nil))

	// Make sure Bob and Alice are connected after his restart.
	net.EnsureConnected(t.t, net.Alice, net.Bob)

	// Send a message from Bob to Alice with a type that Bob is allowed to
	// send, but Alice will not handle as a custom message.
	ctxt, _ = context.WithTimeout(ctx, defaultTimeout)
	_, err = net.Bob.LightningClient.SendCustomMessage(
		ctxt, &lnrpc.SendCustomMessageRequest{
			Peer: net.Alice.PubKey[:],
			Type: overrideType2,
			Data: msgData,
		},
	)
	require.NoError(t.t, err, "bob should be able to send type 2")

	// Do a quick check that Alice did not receive this message in her
	// stream. Note that this is an instant check, so could miss the message
	// being received. We'll also check below that she didn't get it, this
	// is just a sanity check.
	select {
	case msg := <-messages:
		t.t.Fatalf("unexpected message: %v", msg)
	default:
	}

	// Finally, send a custom message with a type that Bob is allowed to
	// send and Alice is configured to receive.
	ctxt, _ = context.WithTimeout(ctx, defaultTimeout)
	_, err = net.Bob.LightningClient.SendCustomMessage(
		ctxt, &lnrpc.SendCustomMessageRequest{
			Peer: net.Alice.PubKey[:],
			Type: overrideType1,
			Data: msgData,
		},
	)
	require.NoError(t.t, err, "bob should be able to send type 1")

	// Wait to receive a message from Bob. This check serves to ensure that
	// our message type 1 was delivered, and assert that the preceding one
	// was not (we could have missed it in our check above). When we receive
	// the second message, we know that the first one did not go through,
	// because we expect our messages to deliver in order.
	select {
	case msg := <-messages:
		// Check our type and data and (sanity) check the peer we got it
		// from.
		require.Equal(t.t, overrideType1, msg.Type, "second message "+
			"type")
		require.Equal(t.t, msgData, msg.Data, "second message data")
		require.Equal(t.t, net.Bob.PubKey[:], msg.Peer, "second "+
			"message peer")

	case <-time.After(defaultTimeout):
		t.t.Fatalf("alice did not receive second custom message")
	}
}
