package itest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/zpay32"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"gopkg.in/macaroon.v2"
)

// testRPCMiddlewareInterceptor tests that the RPC middleware interceptor can
// be used correctly and in a safe way.
func testRPCMiddlewareInterceptor(ht *lntest.HarnessTest) {
	// Let's first enable the middleware interceptor.
	//
	// NOTE: we cannot use standby nodes here as the test messes with
	// middleware interceptor. Thus we also skip the calling of cleanup of
	// each of the following subtests because no standby nodes are used.
	alice := ht.NewNode("alice", []string{"--rpcmiddleware.enable"})
	bob := ht.NewNode("bob", nil)

	// Let's set up a channel between Alice and Bob, just to get some useful
	// data to inspect when doing RPC calls to Alice later.
	ht.EnsureConnected(alice, bob)
	ht.FundCoins(btcutil.SatoshiPerBitcoin, alice)
	ht.OpenChannel(alice, bob, lntest.OpenChannelParams{Amt: 1_234_567})

	// Load or bake the macaroons that the simulated users will use to
	// access the RPC.
	readonlyMac, err := alice.ReadMacaroon(
		alice.Cfg.ReadMacPath, defaultTimeout,
	)
	require.NoError(ht, err)
	adminMac, err := alice.ReadMacaroon(
		alice.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err)

	customCaveatReadonlyMac, err := macaroons.SafeCopyMacaroon(readonlyMac)
	require.NoError(ht, err)
	addConstraint := macaroons.CustomConstraint(
		"itest-caveat", "itest-value",
	)
	require.NoError(ht, addConstraint(customCaveatReadonlyMac))
	customCaveatAdminMac, err := macaroons.SafeCopyMacaroon(adminMac)
	require.NoError(ht, err)
	require.NoError(ht, addConstraint(customCaveatAdminMac))

	// Run all sub-tests now. We can't run anything in parallel because that
	// would cause the main test function to exit and the nodes being
	// cleaned up.
	ht.Run("registration restrictions", func(tt *testing.T) {
		middlewareRegistrationRestrictionTests(tt, alice)
	})

	ht.Run("read-only intercept", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName: "itest-interceptor-1",
				ReadOnlyMode:   true,
			}, true,
		)
		defer registration.cancel()

		middlewareInterceptionTest(
			tt, alice, bob, registration, readonlyMac,
			customCaveatReadonlyMac, true,
		)
	})

	// We've manually disconnected Bob from Alice in the previous test, make
	// sure they're connected again.
	//
	// NOTE: we may get an error here saying "interceptor RPC client quit"
	// as it takes some time for the interceptor to fully quit. Thus we
	// restart the node here to make sure the old interceptor is removed
	// from registration.
	ht.RestartNode(alice)
	ht.EnsureConnected(alice, bob)
	ht.Run("encumbered macaroon intercept", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName:           "itest-interceptor-2",
				CustomMacaroonCaveatName: "itest-caveat",
			}, true,
		)
		defer registration.cancel()

		middlewareInterceptionTest(
			tt, alice, bob, registration,
			customCaveatReadonlyMac, readonlyMac, false,
		)
	})

	// Next, run the response manipulation tests.
	//
	// NOTE: we may get an error here saying "interceptor RPC client quit"
	// as it takes some time for the interceptor to fully quit. Thus we
	// restart the node here to make sure the old interceptor is removed
	// from registration.
	ht.RestartNode(alice)
	ht.EnsureConnected(alice, bob)
	ht.Run("read-only not allowed to manipulate", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName: "itest-interceptor-3",
				ReadOnlyMode:   true,
			}, true,
		)
		defer registration.cancel()

		middlewareRequestManipulationTest(
			tt, alice, registration, adminMac, true,
		)
		middlewareResponseManipulationTest(
			tt, alice, bob, registration, readonlyMac, true,
		)
	})

	// NOTE: we may get an error here saying "interceptor RPC client quit"
	// as it takes some time for the interceptor to fully quit. Thus we
	// restart the node here to make sure the old interceptor is removed
	// from registration.
	ht.RestartNode(alice)
	ht.EnsureConnected(alice, bob)
	ht.Run("encumbered macaroon manipulate", func(tt *testing.T) {
		registration := registerMiddleware(
			tt, alice, &lnrpc.MiddlewareRegistration{
				MiddlewareName:           "itest-interceptor-4",
				CustomMacaroonCaveatName: "itest-caveat",
			}, true,
		)
		defer registration.cancel()

		middlewareRequestManipulationTest(
			tt, alice, registration, customCaveatAdminMac, false,
		)
		middlewareResponseManipulationTest(
			tt, alice, bob, registration,
			customCaveatReadonlyMac, false,
		)
	})

	// And finally make sure mandatory middleware is always checked for any
	// RPC request.
	ht.Run("mandatory middleware", func(tt *testing.T) {
		middlewareMandatoryTest(ht, alice)
	})

	// We now shut down the node manually to prevent the test from failing
	// because we can't call the stop RPC if we unregister the middleware
	// in the defer statement above.
	ht.KillNode(alice)
}

// middlewareRegistrationRestrictionTests tests all restrictions that apply to
// registering a middleware.
func middlewareRegistrationRestrictionTests(t *testing.T,
	node *node.HarnessNode) {

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
				tt, node, tc.registration, false,
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
func middlewareInterceptionTest(t *testing.T,
	node, peer *node.HarnessNode, registration *middlewareHarness,
	userMac *macaroon.Macaroon,
	disallowedMac *macaroon.Macaroon, readOnly bool) {

	t.Helper()

	// Everything we test here should be executed in a matter of
	// milliseconds, so we can use one single timeout context for all calls.
	ctxb := t.Context()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Add some gRPC metadata pairs to the context that we use for one of
	// the calls. This is so that we can test that the interceptor does
	// properly receive the pairs via the interceptor request.
	requestMetadata := metadata.MD{
		"itest-metadata-key": []string{"itest-metadata-value"},
		"itest-metadata-key2": []string{
			"itest-metadata-value1",
			"itest-metadata-value2",
		},
	}
	ctxm := metadata.NewOutgoingContext(ctxc, requestMetadata)

	// Create a client connection that we'll use to simulate user requests
	// to lnd with.
	cleanup, client := macaroonClient(t, node, userMac)
	defer cleanup()

	// We're going to send a simple RPC request to list all channels.
	// We need to invoke the intercept logic in a goroutine because we'd
	// block the execution of the main task otherwise.
	req := &lnrpc.ListChannelsRequest{ActiveOnly: true}
	go registration.interceptUnary(
		"/lnrpc.Lightning/ListChannels", req, nil, readOnly, false,
		requestMetadata,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	resp, err := client.ListChannels(ctxm, req)
	require.NoError(t, err)

	// Did we receive the correct intercept message?
	assertInterceptedType(t, resp, <-registration.responsesChan)

	// Also try the interception of a request that is expected to result in
	// an error being returned from lnd.
	expectedErr := "either `active_only` or `inactive_only` can be set"
	invalidReq := &lnrpc.ListChannelsRequest{
		ActiveOnly:   true,
		InactiveOnly: true,
	}
	go registration.interceptUnary(
		"/lnrpc.Lightning/ListChannels", invalidReq, nil, readOnly,
		false, nil,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	_, err = client.ListChannels(ctxc, invalidReq)
	require.Error(t, err)
	require.Contains(t, err.Error(), expectedErr)

	// Did we receive the correct intercept message with the error?
	assertInterceptedErr(t, expectedErr, <-registration.responsesChan)

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
	peer.RPC.DisconnectPeer(node.PubKeyStr)
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

// middlewareResponseManipulationTest tests that unary and streaming responses
// can be intercepted and also manipulated, at least if the middleware didn't
// register for read-only access.
func middlewareResponseManipulationTest(t *testing.T,
	node, peer *node.HarnessNode, registration *middlewareHarness,
	userMac *macaroon.Macaroon, readOnly bool) {

	t.Helper()

	// Everything we test here should be executed in a matter of
	// milliseconds, so we can use one single timeout context for all calls.
	ctxb := t.Context()
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
		readOnly, false, nil,
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

	// Let's see if we can also replace an error message sent out by lnd
	// with a custom one.
	betterError := fmt.Errorf("yo, this request is no good")
	invalidReq := &lnrpc.ListChannelsRequest{
		ActiveOnly:   true,
		InactiveOnly: true,
	}
	go registration.interceptUnary(
		"/lnrpc.Lightning/ListChannels", invalidReq, betterError,
		readOnly, false, nil,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	_, err = client.ListChannels(ctxc, invalidReq)
	require.Error(t, err)

	// Did we get the manipulated error or the original one?
	if readOnly {
		require.Contains(
			t, err.Error(), "either `active_only` or "+
				"`inactive_only` can be set",
		)
	} else {
		require.Contains(t, err.Error(), betterError.Error())
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
	peer.RPC.DisconnectPeer(node.PubKeyStr)
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

// middlewareRequestManipulationTest tests that unary and streaming requests
// can be intercepted and also manipulated, at least if the middleware didn't
// register for read-only access.
func middlewareRequestManipulationTest(t *testing.T, node *node.HarnessNode,
	registration *middlewareHarness, userMac *macaroon.Macaroon,
	readOnly bool) {

	t.Helper()

	// Everything we test here should be executed in a matter of
	// milliseconds, so we can use one single timeout context for all calls.
	ctxb := t.Context()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Create a client connection that we'll use to simulate user requests
	// to lnd with.
	cleanup, client := macaroonClient(t, node, userMac)
	defer cleanup()

	// We're going to attempt to replace the request with our own. But since
	// we only registered for read-only access, our replacement should just
	// be ignored.
	replacementRequest := &lnrpc.Invoice{
		Memo: "This is the replaced memo",
	}

	// We're going to send a simple RPC request to add an invoice. We need
	// to invoke the intercept logic in a goroutine because we'd block the
	// execution of the main task otherwise.
	req := &lnrpc.Invoice{
		Memo:  "Plz pay me",
		Value: 123456,
	}
	go registration.interceptUnary(
		"/lnrpc.Lightning/AddInvoice", req, replacementRequest,
		readOnly, true, nil,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	resp, err := client.AddInvoice(ctxc, req)
	require.NoError(t, err)

	// Did we get the manipulated response or the original one?
	invoice, err := zpay32.Decode(resp.PaymentRequest, harnessNetParams)
	require.NoError(t, err)
	if readOnly {
		require.Equal(t, req.Memo, *invoice.Description)
	} else {
		require.Equal(t, replacementRequest.Memo, *invoice.Description)
	}

	// Read the message from the registration, otherwise we cannot check the
	// next one.
	<-registration.responsesChan

	// Make sure we also get errors intercepted for stream events. We do
	// this in the request manipulation test because only here we have a
	// macaroon with sufficient permissions (admin macaroon) to attempt to
	// call a streaming RPC that actually accepts request parameters that we
	// can use to provoke an error message.
	expectedErr := "channel is too small"
	invalidReq := &lnrpc.OpenChannelRequest{
		LocalFundingAmount: 5,
	}
	go registration.interceptStream(
		"/lnrpc.Lightning/OpenChannel", invalidReq, nil, readOnly,
	)

	// Do the actual call now and wait for the interceptor to do its thing.
	// We don't receive the error on the RPC call directly, because that
	// only returns the stream. We have to attempt to read a response first
	// to get the expected error.
	responseStream, err := client.OpenChannel(ctxc, invalidReq)
	require.NoError(t, err)

	_, err = responseStream.Recv()
	require.Error(t, err)
	require.Contains(t, err.Error(), expectedErr)

	assertInterceptedErr(t, expectedErr, <-registration.responsesChan)
}

// middlewareMandatoryTest tests that all RPC requests are blocked if there is
// a mandatory middleware declared that's currently not registered.
func middlewareMandatoryTest(ht *lntest.HarnessTest, node *node.HarnessNode) {
	// Let's declare our itest interceptor as mandatory but don't register
	// it just yet. That should cause all RPC requests to fail, except for
	// the registration itself.
	node.Cfg.SkipUnlock = true
	ht.RestartNodeWithExtraArgs(node, []string{
		"--noseedbackup", "--rpcmiddleware.enable",
		"--rpcmiddleware.addmandatory=itest-interceptor",
	})

	// The "wait for node to start" flag of the above restart does too much
	// and has a call to GetInfo built in, which will fail in this special
	// test case. So we need to do the wait and client setup manually here.
	conn, err := node.ConnectRPC()
	require.NoError(ht, err)
	node.Initialize(conn)
	err = node.WaitUntilServerActive()
	require.NoError(ht, err)

	ctxb := ht.Context()
	ctxc, cancel := context.WithTimeout(ctxb, defaultTimeout)
	defer cancel()

	// Test a unary request first.
	_, err = node.RPC.LN.ListChannels(ctxc, &lnrpc.ListChannelsRequest{})
	require.Contains(ht, err.Error(), "middleware 'itest-interceptor' is "+
		"currently not registered")

	// Then a streaming one.
	stream := node.RPC.SubscribeInvoices(&lnrpc.InvoiceSubscription{})
	_, err = stream.Recv()
	require.Error(ht, err)
	require.Contains(ht, err.Error(), "middleware 'itest-interceptor' is "+
		"currently not registered")

	// Now let's register the middleware and try again.
	registration := registerMiddleware(
		ht.T, node, &lnrpc.MiddlewareRegistration{
			MiddlewareName:           "itest-interceptor",
			CustomMacaroonCaveatName: "itest-caveat",
		}, true,
	)
	defer registration.cancel()

	// Both the unary and streaming requests should now be allowed.
	time.Sleep(500 * time.Millisecond)
	node.RPC.ListChannels(&lnrpc.ListChannelsRequest{})
	node.RPC.SubscribeInvoices(&lnrpc.InvoiceSubscription{})
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

// assertInterceptedErr makes sure that the intercept message sent by the RPC
// interceptor contains an error message instead of a serialized proto message.
func assertInterceptedErr(t *testing.T, errString string,
	interceptMessage *lnrpc.RPCMessage) {

	t.Helper()

	require.Equal(t, "error", interceptMessage.TypeName)
	require.True(t, interceptMessage.IsError)

	require.Contains(t, string(interceptMessage.Serialized), errString)
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
func registerMiddleware(t *testing.T, node *node.HarnessNode,
	registration *lnrpc.MiddlewareRegistration,
	waitForRegister bool) *middlewareHarness {

	t.Helper()

	middlewareStream, cancel := node.RPC.RegisterRPCMiddleware()

	errChan := make(chan error)
	go func() {
		msg := &lnrpc.RPCMiddlewareResponse_Register{
			Register: registration,
		}
		err := middlewareStream.Send(&lnrpc.RPCMiddlewareResponse{
			MiddlewareMessage: msg,
		})

		errChan <- err
	}()

	select {
	case <-time.After(defaultTimeout):
		require.Fail(t, "registerMiddleware send timeout")
	case err := <-errChan:
		require.NoError(t, err, "registerMiddleware send failed")
	}

	mh := &middlewareHarness{
		t:             t,
		cancel:        cancel,
		stream:        middlewareStream,
		responsesChan: make(chan *lnrpc.RPCMessage),
	}

	if !waitForRegister {
		return mh
	}

	// Wait for the registration complete message.
	msg := make(chan *lnrpc.RPCMiddlewareRequest)
	go func() {
		regCompleteMsg, err := middlewareStream.Recv()
		require.NoError(t, err, "registerMiddleware recv failed")

		msg <- regCompleteMsg
	}()

	select {
	case <-time.After(defaultTimeout):
		require.Fail(t, "registerMiddleware recv timeout")

	case m := <-msg:
		require.True(t, m.GetRegComplete())
	}

	return mh
}

// interceptUnary intercepts a unary call, optionally requesting to replace the
// response sent to the client. A unary call is expected to receive one
// intercept message for the request and one for the response.
//
// NOTE: Must be called in a goroutine as this will block until the response is
// read from the response channel.
func (h *middlewareHarness) interceptUnary(methodURI string,
	expectedRequest proto.Message, responseReplacement interface{},
	readOnly bool, replaceRequest bool, expectedMetadata metadata.MD) {

	// Read intercept message and make sure it's for an RPC request.
	reqIntercept, err := h.stream.Recv()
	require.NoError(h.t, err)

	// Check that we have the expected metadata in the request.
	if len(expectedMetadata) > 0 {
		require.GreaterOrEqual(
			h.t, len(reqIntercept.MetadataPairs),
			len(expectedMetadata),
		)

		for key := range expectedMetadata {
			require.Contains(h.t, reqIntercept.MetadataPairs, key)
			require.Equal(
				h.t, expectedMetadata[key],
				reqIntercept.MetadataPairs[key].Values,
			)
		}
	}

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
	if replaceRequest {
		h.sendAccept(reqIntercept.MsgId, responseReplacement)
	} else {
		h.sendAccept(reqIntercept.MsgId, nil)
	}

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
	if replaceRequest {
		h.sendAccept(respIntercept.MsgId, nil)
	} else {
		h.sendAccept(respIntercept.MsgId, responseReplacement)
	}

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
	responseReplacement interface{}) {

	var replacementBytes []byte
	if responseReplacement != nil {
		var err error

		switch t := responseReplacement.(type) {
		case proto.Message:
			replacementBytes, err = proto.Marshal(t)
			require.NoError(h.t, err)

		case error:
			replacementBytes = []byte(t.Error())

		default:
			require.Fail(h.t, "invalid replacement type")
		}
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
