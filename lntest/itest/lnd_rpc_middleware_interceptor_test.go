package itest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon.v2"
)

// testRPCMiddlewareInterceptor tests that the RPC middleware interceptor can
// be used correctly and in a safe way.
func testRPCMiddlewareInterceptor(net *lntest.NetworkHarness, t *harnessTest) {
	// Let's first enable the middleware interceptor.
	net.Alice.Cfg.ExtraArgs = append(
		net.Alice.Cfg.ExtraArgs, "--rpcmiddleware.enable",
	)
	err := net.RestartNode(net.Alice, nil)
	require.NoError(t.t, err)

	// Let's set up a channel between Alice and Bob, just to get some useful
	// data to inspect when doing RPC calls to Alice later.
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	net.SendCoins(t.t, btcutil.SatoshiPerBitcoin, net.Alice)
	_ = openChannelAndAssert(
		t, net, net.Alice, net.Bob, lntest.OpenChannelParams{
			Amt: 1_234_567,
		},
	)

	// Load or bake the macaroons that the simulated users will use to
	// access the RPC.
	readonlyMac, err := net.Alice.ReadMacaroon(
		net.Alice.ReadMacPath(), defaultTimeout,
	)
	require.NoError(t.t, err)

	customCaveatMac, err := macaroons.SafeCopyMacaroon(readonlyMac)
	require.NoError(t.t, err)
	addConstraint := macaroons.CustomConstraint(
		"itest-caveat", "itest-value",
	)
	require.NoError(t.t, addConstraint(customCaveatMac))

	// Run all sub-tests now. We can't run anything in parallel because that
	// would cause the main test function to exit and the nodes being
	// cleaned up.
	t.t.Run("registration restrictions", func(tt *testing.T) {
		middlewareRegistrationRestrictionTests(tt, net.Alice)
	})
	t.t.Run("read-only intercept", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, net.Alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName: "itest-interceptor",
				ReadOnlyMode:   true,
			},
		)
		defer registration.cancel()

		middlewareInterceptionTest(
			tt, net.Alice, net.Bob, registration, readonlyMac,
			customCaveatMac, true,
		)
	})

	// We've manually disconnected Bob from Alice in the previous test, make
	// sure they're connected again.
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	t.t.Run("encumbered macaroon intercept", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, net.Alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName:           "itest-interceptor",
				CustomMacaroonCaveatName: "itest-caveat",
			},
		)
		defer registration.cancel()

		middlewareInterceptionTest(
			tt, net.Alice, net.Bob, registration, customCaveatMac,
			readonlyMac, false,
		)
	})

	// Next, run the response manipulation tests.
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	t.t.Run("read-only not allowed to manipulate", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, net.Alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName: "itest-interceptor",
				ReadOnlyMode:   true,
			},
		)
		defer registration.cancel()

		middlewareManipulationTest(
			tt, net.Alice, net.Bob, registration, readonlyMac, true,
		)
	})
	net.EnsureConnected(t.t, net.Alice, net.Bob)
	t.t.Run("encumbered macaroon manipulate", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, net.Alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName:           "itest-interceptor",
				CustomMacaroonCaveatName: "itest-caveat",
			},
		)
		defer registration.cancel()

		middlewareManipulationTest(
			tt, net.Alice, net.Bob, registration, customCaveatMac,
			false,
		)
	})

	// And finally make sure mandatory middleware is always checked for any
	// RPC request.
	t.t.Run("mandatory middleware", func(tt *testing.T) {
		middlewareMandatoryTest(tt, net.Alice, net)
	})
}

// middlewareRegistrationRestrictionTests tests all restrictions that apply to
// registering a middleware.
func middlewareRegistrationRestrictionTests(t *testing.T,
	node *lntest.HarnessNode) {

	testCases := []struct {
		registration *lnrpc.MiddlewareRegistration
		expectedErr  string
	}{{
		registration: &lnrpc.MiddlewareRegistration{
			MiddlewareName: "foo",
		},
		expectedErr: "invalid middleware name",
	}, {
		registration: &lnrpc.MiddlewareRegistration{
			MiddlewareName:           "itest-interceptor",
			CustomMacaroonCaveatName: "foo",
		},
		expectedErr: "custom caveat name of at least",
	}, {
		registration: &lnrpc.MiddlewareRegistration{
			MiddlewareName:           "itest-interceptor",
			CustomMacaroonCaveatName: "itest-caveat",
			ReadOnlyMode:             true,
		},
		expectedErr: "cannot set read-only and custom caveat name",
	}}

	for idx, tc := range testCases {
		tc := tc

		t.Run(fmt.Sprintf("%d", idx), func(tt *testing.T) {
			invalidName := registerMiddleware(
				tt, node, tc.registration,
			)
			_, err := invalidName.stream.Recv()
			require.Error(tt, err)
			require.Contains(tt, err.Error(), tc.expectedErr)

			invalidName.cancel()
		})
	}
}

// middlewareInterceptionTest tests that unary and streaming requests can be
// intercepted. It also makes sure that depending on the mode (read-only or
// custom macaroon caveat) a middleware only gets access to the requests it
// should be allowed access to.
func middlewareInterceptionTest(t *testing.T, node *lntest.HarnessNode,
	peer *lntest.HarnessNode, registration *middlewareHarness,
	userMac *macaroon.Macaroon, disallowedMac *macaroon.Macaroon,
	readOnly bool) {

	// Everything we test here should be executed in a matter of
	// milliseconds, so we can use one single timeout context for all calls.
	ctxb := context.Background()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Create a client connection that we'll use to simulate user requests
	// to lnd with.
	cleanup, client := macaroonClient(t, node, userMac)
	defer cleanup()

	// We're going to send a simple RPC request to list all channels.
	// We need to invoke the intercept logic in a goroutine because we'd
	// block the execution of the main task otherwise.
	req := &lnrpc.ListChannelsRequest{ActiveOnly: true}
	go registration.interceptUnary(
		"/lnrpc.Lightning/ListChannels", req, nil, readOnly,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	resp, err := client.ListChannels(ctxc, req)
	require.NoError(t, err)

	// Did we receive the correct intercept message?
	assertInterceptedType(t, resp, <-registration.responsesChan)

	// Let's test the same for a streaming endpoint.
	req2 := &lnrpc.PeerEventSubscription{}
	go registration.interceptStream(
		"/lnrpc.Lightning/SubscribePeerEvents", req2, nil, readOnly,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	peerCtx, peerCancel := context.WithCancel(ctxb)
	resp2, err := client.SubscribePeerEvents(peerCtx, req2)
	require.NoError(t, err)

	// Disconnect Bob to trigger a peer event without using Alice's RPC
	// interface itself.
	_, err = peer.DisconnectPeer(ctxc, &lnrpc.DisconnectPeerRequest{
		PubKey: node.PubKeyStr,
	})
	require.NoError(t, err)
	peerEvent, err := resp2.Recv()
	require.NoError(t, err)
	require.Equal(t, lnrpc.PeerEvent_PEER_OFFLINE, peerEvent.GetType())

	// Stop the peer stream again, otherwise we'll produce more events.
	peerCancel()

	// Did we receive the correct intercept message?
	assertInterceptedType(t, peerEvent, <-registration.responsesChan)

	// Make sure that with the other macaroon we aren't allowed to access
	// the interceptor. If we registered for read-only access then there is
	// no middleware that handles the custom macaroon caveat. If we
	// registered for a custom caveat then there is no middleware that
	// handles unencumbered read-only access.
	cleanup, client = macaroonClient(t, node, disallowedMac)
	defer cleanup()

	// We need to make sure we don't get any interception messages for
	// requests made with the disallowed macaroon.
	var (
		errChan = make(chan error, 1)
		msgChan = make(chan *lnrpc.RPCMiddlewareRequest, 1)
	)
	go func() {
		req, err := registration.stream.Recv()
		if err != nil {
			errChan <- err
			return
		}

		msgChan <- req
	}()

	// Let's invoke the same request again but with the other macaroon.
	resp, err = client.ListChannels(ctxc, req)

	// Depending on what mode we're in, we expect something different. If we
	// are in read-only mode then an encumbered macaroon cannot be used
	// since there is no middleware registered for it. If we registered for
	// a custom macaroon caveat and a request with anon-encumbered macaroon
	// comes in, we expect to just not get any intercept messages.
	if readOnly {
		require.Error(t, err)
		require.Contains(
			t, err.Error(), "cannot accept macaroon with custom "+
				"caveat 'itest-caveat', no middleware "+
				"registered",
		)
	} else {
		require.NoError(t, err)

		// We disconnected Bob so there should be no active channels.
		require.Len(t, resp.Channels, 0)
	}

	// There should be neither an error nor any interception messages in the
	// channels.
	select {
	case err := <-errChan:
		t.Fatalf("Unexpected error, not expecting messages: %v", err)

	case msg := <-msgChan:
		t.Fatalf("Unexpected intercept message: %v", msg)

	case <-time.After(time.Second):
		// Nothing came in for a second, we're fine.
	}
}

// middlewareManipulationTest tests that unary and streaming requests can be
// intercepted and also manipulated, at least if the middleware didn't register
// for read-only access.
func middlewareManipulationTest(t *testing.T, node *lntest.HarnessNode,
	peer *lntest.HarnessNode, registration *middlewareHarness,
	userMac *macaroon.Macaroon, readOnly bool) {

	// Everything we test here should be executed in a matter of
	// milliseconds, so we can use one single timeout context for all calls.
	ctxb := context.Background()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Create a client connection that we'll use to simulate user requests
	// to lnd with.
	cleanup, client := macaroonClient(t, node, userMac)
	defer cleanup()

	// We're going to attempt to replace the response with our own. But
	// since we only registered for read-only access, our replacement should
	// just be ignored.
	replacementResponse := &lnrpc.ListChannelsResponse{
		Channels: []*lnrpc.Channel{{
			ChannelPoint: "f000:0",
		}, {
			ChannelPoint: "f000:1",
		}},
	}

	// We're going to send a simple RPC request to list all channels.
	// We need to invoke the intercept logic in a goroutine because we'd
	// block the execution of the main task otherwise.
	req := &lnrpc.ListChannelsRequest{ActiveOnly: true}
	go registration.interceptUnary(
		"/lnrpc.Lightning/ListChannels", req, replacementResponse,
		readOnly,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	resp, err := client.ListChannels(ctxc, req)
	require.NoError(t, err)

	// Did we get the manipulated response (2 fake channels) or the original
	// one (1 channel)?
	if readOnly {
		require.Len(t, resp.Channels, 1)
	} else {
		require.Len(t, resp.Channels, 2)
	}

	// Let's test the same for a streaming endpoint.
	replacementResponse2 := &lnrpc.PeerEvent{
		Type:   lnrpc.PeerEvent_PEER_ONLINE,
		PubKey: "foo",
	}
	req2 := &lnrpc.PeerEventSubscription{}
	go registration.interceptStream(
		"/lnrpc.Lightning/SubscribePeerEvents", req2,
		replacementResponse2, readOnly,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	peerCtx, peerCancel := context.WithCancel(ctxb)
	resp2, err := client.SubscribePeerEvents(peerCtx, req2)
	require.NoError(t, err)

	// Disconnect Bob to trigger a peer event without using Alice's RPC
	// interface itself.
	_, err = peer.DisconnectPeer(ctxc, &lnrpc.DisconnectPeerRequest{
		PubKey: node.PubKeyStr,
	})
	require.NoError(t, err)
	peerEvent, err := resp2.Recv()
	require.NoError(t, err)

	// Did we get the correct, original response?
	if readOnly {
		require.Equal(
			t, lnrpc.PeerEvent_PEER_OFFLINE, peerEvent.GetType(),
		)
		require.Equal(t, peer.PubKeyStr, peerEvent.PubKey)
	} else {
		require.Equal(
			t, lnrpc.PeerEvent_PEER_ONLINE, peerEvent.GetType(),
		)
		require.Equal(t, "foo", peerEvent.PubKey)
	}

	// Stop the peer stream again, otherwise we'll produce more events.
	peerCancel()
}

// middlewareMandatoryTest tests that all RPC requests are blocked if there is
// a mandatory middleware declared that's currently not registered.
func middlewareMandatoryTest(t *testing.T, node *lntest.HarnessNode,
	net *lntest.NetworkHarness) {

	// Let's declare our itest interceptor as mandatory but don't register
	// it just yet. That should cause all RPC requests to fail, except for
	// the registration itself.
	node.Cfg.ExtraArgs = append(
		node.Cfg.ExtraArgs,
		"--rpcmiddleware.addmandatory=itest-interceptor",
	)
	err := net.RestartNodeNoUnlock(node, nil, false)
	require.NoError(t, err)

	// The "wait for node to start" flag of the above restart does too much
	// and has a call to GetInfo built in, which will fail in this special
	// test case. So we need to do the wait and client setup manually here.
	conn, err := node.ConnectRPC(true)
	require.NoError(t, err)
	node.InitRPCClients(conn)
	err = node.WaitUntilStateReached(lnrpc.WalletState_RPC_ACTIVE)
	require.NoError(t, err)
	node.LightningClient = lnrpc.NewLightningClient(conn)

	ctxb := context.Background()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Test a unary request first.
	_, err = node.ListChannels(ctxc, &lnrpc.ListChannelsRequest{})
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "middleware 'itest-interceptor' is "+
			"currently not registered",
	)

	// Then a streaming one.
	stream, err := node.SubscribeInvoices(ctxc, &lnrpc.InvoiceSubscription{})
	require.NoError(t, err)
	_, err = stream.Recv()
	require.Error(t, err)
	require.Contains(
		t, err.Error(), "middleware 'itest-interceptor' is "+
			"currently not registered",
	)

	// Now let's register the middleware and try again.
	registration := registerMiddleware(
		t, node, &lnrpc.MiddlewareRegistration{
			MiddlewareName:           "itest-interceptor",
			CustomMacaroonCaveatName: "itest-caveat",
		},
	)
	defer registration.cancel()

	// Both the unary and streaming requests should now be allowed.
	time.Sleep(500 * time.Millisecond)
	_, err = node.ListChannels(ctxc, &lnrpc.ListChannelsRequest{})
	require.NoError(t, err)
	_, err = node.SubscribeInvoices(ctxc, &lnrpc.InvoiceSubscription{})
	require.NoError(t, err)

	// We now shut down the node manually to prevent the test from failing
	// because we can't call the stop RPC if we unregister the middleware in
	// the defer statement above.
	err = net.ShutdownNode(node)
	require.NoError(t, err)
}

// assertInterceptedType makes sure that the intercept message sent by the RPC
// interceptor is correct for a proto message that was sent or received over the
// RPC interface.
func assertInterceptedType(t *testing.T, rpcMessage proto.Message,
	interceptMessage *lnrpc.RPCMessage) {

	t.Helper()

	require.Equal(
		t, string(proto.MessageName(rpcMessage)),
		interceptMessage.TypeName,
	)
	rawRequest, err := proto.Marshal(rpcMessage)
	require.NoError(t, err)

	// Make sure we don't trip over nil vs. empty slice in the equality
	// check below.
	if len(rawRequest) == 0 {
		rawRequest = nil
	}

	require.Equal(t, rawRequest, interceptMessage.Serialized)
}

// middlewareStream is a type alias to shorten the long definition.
type middlewareStream lnrpc.Lightning_RegisterRPCMiddlewareClient

// middlewareHarness is a test harness that holds one instance of a simulated
// middleware.
type middlewareHarness struct {
	t      *testing.T
	cancel func()
	stream middlewareStream

	responsesChan chan *lnrpc.RPCMessage
}

// registerMiddleware creates a new middleware harness and sends the initial
// register message to the RPC server.
func registerMiddleware(t *testing.T, node *lntest.HarnessNode,
	registration *lnrpc.MiddlewareRegistration) *middlewareHarness {

	ctxc, cancel := context.WithCancel(context.Background())

	middlewareStream, err := node.RegisterRPCMiddleware(ctxc)
	require.NoError(t, err)

	err = middlewareStream.Send(&lnrpc.RPCMiddlewareResponse{
		MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Register{
			Register: registration,
		},
	})
	require.NoError(t, err)

	return &middlewareHarness{
		t:             t,
		cancel:        cancel,
		stream:        middlewareStream,
		responsesChan: make(chan *lnrpc.RPCMessage),
	}
}

// interceptUnary intercepts a unary call, optionally requesting to replace the
// response sent to the client. A unary call is expected to receive one
// intercept message for the request and one for the response.
//
// NOTE: Must be called in a goroutine as this will block until the response is
// read from the response channel.
func (h *middlewareHarness) interceptUnary(methodURI string,
	expectedRequest proto.Message, responseReplacement proto.Message,
	readOnly bool) {

	// Read intercept message and make sure it's for an RPC request.
	reqIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)

	// Make sure the custom condition is populated correctly (if we're using
	// a macaroon with a custom condition).
	if !readOnly {
		require.Equal(
			h.t, "itest-value", reqIntercept.CustomCaveatCondition,
		)
	}

	req := reqIntercept.GetRequest()
	require.NotNil(h.t, req)

	// We know the request we're going to send so make sure we get the right
	// type and content from the interceptor.
	require.Equal(h.t, methodURI, req.MethodFullUri)
	assertInterceptedType(h.t, expectedRequest, req)

	// We need to accept the request.
	h.sendAccept(reqIntercept.MsgId, nil)

	// Now read the intercept message for the response.
	respIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)
	res := respIntercept.GetResponse()
	require.NotNil(h.t, res)

	// We expect the request ID to be the same for the request intercept
	// and the response intercept messages. But the message IDs must be
	// different/unique.
	require.Equal(h.t, reqIntercept.RequestId, respIntercept.RequestId)
	require.NotEqual(h.t, reqIntercept.MsgId, respIntercept.MsgId)

	// We need to accept the response as well.
	h.sendAccept(respIntercept.MsgId, responseReplacement)

	h.responsesChan <- res
}

// interceptStream intercepts a streaming call, optionally requesting to replace
// the (first) response sent to the client. A streaming call is expected to
// receive one intercept message for the stream authentication, one for the
// first request and one for the first response.
//
// NOTE: Must be called in a goroutine as this will block until the first
// response is read from the response channel.
func (h *middlewareHarness) interceptStream(methodURI string,
	expectedRequest proto.Message, responseReplacement proto.Message,
	readOnly bool) {

	// Read intercept message and make sure it's for an RPC stream auth.
	authIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)

	// Make sure the custom condition is populated correctly (if we're using
	// a macaroon with a custom condition).
	if !readOnly {
		require.Equal(
			h.t, "itest-value", authIntercept.CustomCaveatCondition,
		)
	}

	auth := authIntercept.GetStreamAuth()
	require.NotNil(h.t, auth)

	// This is just the authentication, so we can only look at the URI.
	require.Equal(h.t, methodURI, auth.MethodFullUri)

	// We need to accept the auth.
	h.sendAccept(authIntercept.MsgId, nil)

	// Read intercept message and make sure it's for an RPC request.
	reqIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)
	req := reqIntercept.GetRequest()
	require.NotNil(h.t, req)

	// We know the request we're going to send so make sure we get the right
	// type and content from the interceptor.
	require.Equal(h.t, methodURI, req.MethodFullUri)
	assertInterceptedType(h.t, expectedRequest, req)

	// We need to accept the request.
	h.sendAccept(reqIntercept.MsgId, nil)

	// Now read the intercept message for the response.
	respIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)
	res := respIntercept.GetResponse()
	require.NotNil(h.t, res)

	// We expect the request ID to be the same for the auth intercept,
	// request intercept and the response intercept messages. But the
	// message IDs must be different/unique.
	require.Equal(h.t, authIntercept.RequestId, respIntercept.RequestId)
	require.Equal(h.t, reqIntercept.RequestId, respIntercept.RequestId)
	require.NotEqual(h.t, authIntercept.MsgId, reqIntercept.MsgId)
	require.NotEqual(h.t, authIntercept.MsgId, respIntercept.MsgId)
	require.NotEqual(h.t, reqIntercept.MsgId, respIntercept.MsgId)

	// We need to accept the response as well.
	h.sendAccept(respIntercept.MsgId, responseReplacement)

	h.responsesChan <- res
}

// sendAccept sends an accept feedback to the RPC server.
func (h *middlewareHarness) sendAccept(msgID uint64,
	responseReplacement proto.Message) {

	var replacementBytes []byte
	if responseReplacement != nil {
		var err error
		replacementBytes, err = proto.Marshal(responseReplacement)
		require.NoError(h.t, err)
	}

	err := h.stream.Send(&lnrpc.RPCMiddlewareResponse{
		MiddlewareMessage: &lnrpc.RPCMiddlewareResponse_Feedback{
			Feedback: &lnrpc.InterceptFeedback{
				ReplaceResponse:       len(replacementBytes) > 0,
				ReplacementSerialized: replacementBytes,
			},
		},
		RefMsgId: msgID,
	})
	require.NoError(h.t, err)
}
