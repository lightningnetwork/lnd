package itest

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/lightningnetwork/lnd/lntest/node"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

var (
	insecureTransport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient = &http.Client{
		Transport: insecureTransport,
	}
	urlEnc          = base64.URLEncoding
	webSocketDialer = &websocket.Dialer{
		HandshakeTimeout: time.Second,
		TLSClientConfig:  insecureTransport.TLSClientConfig,
	}
	resultPattern = regexp.MustCompile("{\"result\":(.*)}")
	closeMsg      = websocket.FormatCloseMessage(
		websocket.CloseNormalClosure, "done",
	)

	pingInterval = time.Millisecond * 200
	pongWait     = time.Millisecond * 50
)

// testRestAPI tests that the most important features of the REST API work
// correctly.
func testRestAPI(ht *lntest.HarnessTest) {
	testCases := []struct {
		name string
		run  func(*testing.T, *node.HarnessNode, *node.HarnessNode)
	}{{
		name: "simple GET",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			// Check that the parsing into the response proto
			// message works.
			resp := &lnrpc.GetInfoResponse{}
			err := invokeGET(a, "/v1/getinfo", resp)
			require.Nil(t, err, "getinfo")
			require.Equal(t, "#3399ff", resp.Color, "node color")

			// Make sure we get the correct field names (snake
			// case).
			_, resp2, err := makeRequest(
				a, "/v1/getinfo", "GET", nil, nil,
			)
			require.Nil(t, err, "getinfo")
			require.Contains(
				t, string(resp2), "best_header_timestamp",
				"getinfo",
			)
		},
	}, {
		name: "simple POST and GET with query param",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			// Add an invoice, testing POST in the process.
			req := &lnrpc.Invoice{Value: 1234}
			resp := &lnrpc.AddInvoiceResponse{}
			err := invokePOST(a, "/v1/invoices", req, resp)
			require.Nil(t, err, "add invoice")
			require.Equal(t, 32, len(resp.RHash), "invoice rhash")

			// Make sure we can call a GET endpoint with a hex
			// encoded URL part.
			url := fmt.Sprintf("/v1/invoice/%x", resp.RHash)
			resp2 := &lnrpc.Invoice{}
			err = invokeGET(a, url, resp2)
			require.Nil(t, err, "query invoice")
			require.Equal(t, int64(1234), resp2.Value,
				"invoice amt")
		},
	}, {
		name: "GET with base64 encoded byte slice in path",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			url := "/v2/router/mc/probability/%s/%s/%d"
			url = fmt.Sprintf(
				url, urlEnc.EncodeToString(a.PubKey[:]),
				urlEnc.EncodeToString(b.PubKey[:]), 1234,
			)
			resp := &routerrpc.QueryProbabilityResponse{}
			err := invokeGET(a, url, resp)
			require.Nil(t, err, "query probability")
			require.Zero(t, resp.Probability)
		},
	}, {
		name: "GET with map type query param",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			// Use a fake address.
			addr := "bcrt1qlutnwklt4u2548cufrjmsjclewugr9lcpnkzag"

			// Create the full URL with the map query param.
			url := "/v1/transactions/fee?target_conf=%d&" +
				"AddrToAmount[%s]=%d"
			url = fmt.Sprintf(url, 2, addr, 50000)
			resp := &lnrpc.EstimateFeeResponse{}
			err := invokeGET(a, url, resp)
			require.Nil(t, err, "estimate fee")
			require.Greater(t, resp.FeeSat, int64(253), "fee")
		},
	}, {
		name: "sub RPC servers REST support",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			// Query autopilot status.
			res1 := &autopilotrpc.StatusResponse{}
			err := invokeGET(a, "/v2/autopilot/status", res1)
			require.Nil(t, err, "autopilot status")
			require.Equal(t, false, res1.Active, "autopilot status")

			// Query the version RPC.
			res2 := &verrpc.Version{}
			err = invokeGET(a, "/v2/versioner/version", res2)
			require.Nil(t, err, "version")
			require.Greater(
				t, res2.AppMinor, uint32(0), "lnd minor version",
			)

			// Request a new external address from the wallet kit.
			req1 := &walletrpc.AddrRequest{}
			res3 := &walletrpc.AddrResponse{}
			err = invokePOST(
				a, "/v2/wallet/address/next", req1, res3,
			)
			require.Nil(t, err, "address")
			require.NotEmpty(t, res3.Addr, "address")
		},
	}, {
		name: "CORS headers",
		run: func(t *testing.T, a, b *node.HarnessNode) {
			t.Helper()

			// Alice allows all origins. Make sure we get the same
			// value back in the CORS header that we send in the
			// Origin header.
			reqHeaders := make(http.Header)
			reqHeaders.Add("Origin", "https://foo.bar:9999")
			resHeaders, body, err := makeRequest(
				a, "/v1/getinfo", "OPTIONS", nil, reqHeaders,
			)
			require.Nil(t, err, "getinfo")
			require.Equal(
				t, "https://foo.bar:9999",
				resHeaders.Get("Access-Control-Allow-Origin"),
				"CORS header",
			)
			require.Equal(t, 0, len(body))

			// Make sure that we don't get a value set for Bob which
			// doesn't allow any CORS origin.
			resHeaders, body, err = makeRequest(
				b, "/v1/getinfo", "OPTIONS", nil, reqHeaders,
			)
			require.Nil(t, err, "getinfo")
			require.Equal(
				t, "",
				resHeaders.Get("Access-Control-Allow-Origin"),
				"CORS header",
			)
			require.Equal(t, 0, len(body))
		},
	}}
	wsTestCases := []struct {
		name string
		run  func(ht *lntest.HarnessTest)
	}{{
		name: "websocket subscription",
		run:  wsTestCaseSubscription,
	}, {
		name: "websocket subscription with macaroon in protocol",
		run:  wsTestCaseSubscriptionMacaroon,
	}, {
		name: "websocket bi-directional subscription",
		run:  wsTestCaseBiDirectionalSubscription,
	}, {
		name: "websocket ping and pong timeout",
		run:  wsTestPingPongTimeout,
	}}

	// Make sure Alice allows all CORS origins. Bob will keep the default.
	// We also make sure the ping/pong messages are sent very often, so we
	// can test them without waiting half a minute.
	bob := ht.NewNode("Bob", nil)
	args := []string{
		"--restcors=\"*\"",
		fmt.Sprintf("--ws-ping-interval=%s", pingInterval),
		fmt.Sprintf("--ws-pong-wait=%s", pongWait),
	}
	alice := ht.NewNodeWithCoins("Alice", args)

	for _, tc := range testCases {
		tc := tc
		ht.Run(tc.name, func(t *testing.T) {
			tc.run(t, alice, bob)
		})
	}

	for _, tc := range wsTestCases {
		tc := tc
		ht.Run(tc.name, func(t *testing.T) {
			st := ht.Subtest(t)
			tc.run(st)
		})
	}
}

func wsTestCaseSubscription(ht *lntest.HarnessTest) {
	alice := ht.NewNode("Alice", nil)

	// Find out the current best block so we can subscribe to the next one.
	hash, height := ht.GetBestBlock()

	// Create a new subscription to get block epoch events.
	req := &chainrpc.BlockEpoch{
		Hash:   hash.CloneBytes(),
		Height: uint32(height),
	}
	url := "/v2/chainnotifier/register/blocks"
	c, err := openWebSocket(alice, url, "POST", req, nil)
	require.NoError(ht, err, "websocket")
	defer func() {
		err := c.WriteMessage(websocket.CloseMessage, closeMsg)
		require.NoError(ht, err)
		_ = c.Close()
	}()

	msgChan := make(chan *chainrpc.BlockEpoch)
	errChan := make(chan error)
	timeout := time.After(defaultTimeout)

	// We want to read exactly one message.
	go func() {
		defer close(msgChan)

		_, msg, err := c.ReadMessage()
		if err != nil {
			errChan <- err
			return
		}

		// The chunked/streamed responses come wrapped in either a
		// {"result":{}} or {"error":{}} wrapper which we'll get rid of
		// here.
		msgStr := string(msg)
		if !strings.Contains(msgStr, "\"result\":") {
			errChan <- fmt.Errorf("invalid msg: %s", msgStr)
			return
		}
		msgStr = resultPattern.ReplaceAllString(msgStr, "${1}")

		// Make sure we can parse the unwrapped message into the
		// expected proto message.
		protoMsg := &chainrpc.BlockEpoch{}
		err = lnrpc.RESTJsonUnmarshalOpts.Unmarshal(
			[]byte(msgStr), protoMsg,
		)
		if err != nil {
			errChan <- err
			return
		}

		select {
		case msgChan <- protoMsg:
		case <-timeout:
		}
	}()

	// Mine a block and make sure we get a message for it.
	blockHashes := ht.Miner().GenerateBlocks(1)
	select {
	case msg := <-msgChan:
		require.Equal(
			ht, blockHashes[0].CloneBytes(), msg.Hash,
			"block hash",
		)

	case err := <-errChan:
		ht.Fatalf("Received error from WS: %v", err)

	case <-timeout:
		ht.Fatalf("Timeout before message was received")
	}
}

func wsTestCaseSubscriptionMacaroon(ht *lntest.HarnessTest) {
	// Find out the current best block so we can subscribe to the next one.
	hash, height := ht.GetBestBlock()

	// Create a new subscription to get block epoch events.
	req := &chainrpc.BlockEpoch{
		Hash:   hash.CloneBytes(),
		Height: uint32(height),
	}
	url := "/v2/chainnotifier/register/blocks"

	// This time we send the macaroon in the special header
	// Sec-Websocket-Protocol which is the only header field available to
	// browsers when opening a WebSocket.
	alice := ht.NewNode("Alice", nil)
	mac, err := alice.ReadMacaroon(
		alice.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err, "read admin mac")
	macBytes, err := mac.MarshalBinary()
	require.NoError(ht, err, "marshal admin mac")

	customHeader := make(http.Header)
	customHeader.Set(lnrpc.HeaderWebSocketProtocol, fmt.Sprintf(
		"Grpc-Metadata-Macaroon+%s", hex.EncodeToString(macBytes),
	))
	c, err := openWebSocket(alice, url, "POST", req, customHeader)
	require.Nil(ht, err, "websocket")
	defer func() {
		err := c.WriteMessage(websocket.CloseMessage, closeMsg)
		require.NoError(ht, err)
		_ = c.Close()
	}()

	msgChan := make(chan *chainrpc.BlockEpoch)
	errChan := make(chan error)
	timeout := time.After(defaultTimeout)

	// We want to read exactly one message.
	go func() {
		defer close(msgChan)

		_, msg, err := c.ReadMessage()
		if err != nil {
			errChan <- err
			return
		}

		// The chunked/streamed responses come wrapped in either a
		// {"result":{}} or {"error":{}} wrapper which we'll get rid of
		// here.
		msgStr := string(msg)
		if !strings.Contains(msgStr, "\"result\":") {
			errChan <- fmt.Errorf("invalid msg: %s", msgStr)
			return
		}
		msgStr = resultPattern.ReplaceAllString(msgStr, "${1}")

		// Make sure we can parse the unwrapped message into the
		// expected proto message.
		protoMsg := &chainrpc.BlockEpoch{}
		err = lnrpc.RESTJsonUnmarshalOpts.Unmarshal(
			[]byte(msgStr), protoMsg,
		)
		if err != nil {
			errChan <- err
			return
		}

		select {
		case msgChan <- protoMsg:
		case <-timeout:
		}
	}()

	// Mine a block and make sure we get a message for it.
	blockHashes := ht.Miner().GenerateBlocks(1)
	select {
	case msg := <-msgChan:
		require.Equal(
			ht, blockHashes[0].CloneBytes(), msg.Hash,
			"block hash",
		)

	case err := <-errChan:
		ht.Fatalf("Received error from WS: %v", err)

	case <-timeout:
		ht.Fatalf("Timeout before message was received")
	}
}

func wsTestCaseBiDirectionalSubscription(ht *lntest.HarnessTest) {
	initialRequest := &lnrpc.ChannelAcceptResponse{}
	url := "/v1/channels/acceptor"

	// This time we send the macaroon in the special header
	// Sec-Websocket-Protocol which is the only header field available to
	// browsers when opening a WebSocket.
	alice := ht.NewNode("Alice", nil)
	mac, err := alice.ReadMacaroon(
		alice.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err, "read admin mac")
	macBytes, err := mac.MarshalBinary()
	require.NoError(ht, err, "marshal admin mac")

	customHeader := make(http.Header)
	customHeader.Set(lnrpc.HeaderWebSocketProtocol, fmt.Sprintf(
		"Grpc-Metadata-Macaroon+%s", hex.EncodeToString(macBytes),
	))
	conn, err := openWebSocket(
		alice, url, "POST", initialRequest, customHeader,
	)
	require.Nil(ht, err, "websocket")
	defer func() {
		err := conn.WriteMessage(websocket.CloseMessage, closeMsg)
		_ = conn.Close()
		require.NoError(ht, err)
	}()

	// Buffer the message channel to make sure we're always blocking on
	// conn.ReadMessage() to allow the ping/pong mechanism to work.
	msgChan := make(chan *lnrpc.ChannelAcceptResponse, 1)
	errChan := make(chan error)
	done := make(chan struct{})

	// We want to read messages over and over again. We just accept any
	// channels that are opened.
	defer close(done)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}

			// The chunked/streamed responses come wrapped in either
			// a {"result":{}} or {"error":{}} wrapper which we'll
			// get rid of here.
			msgStr := string(msg)
			if !strings.Contains(msgStr, "\"result\":") {
				select {
				case errChan <- fmt.Errorf("invalid msg: %s",
					msgStr):
				case <-done:
				}
				return
			}
			msgStr = resultPattern.ReplaceAllString(msgStr, "${1}")

			// Make sure we can parse the unwrapped message into the
			// expected proto message.
			protoMsg := &lnrpc.ChannelAcceptRequest{}
			err = lnrpc.RESTJsonUnmarshalOpts.Unmarshal(
				[]byte(msgStr), protoMsg,
			)
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}

			// Send the response that we accept the channel.
			res := &lnrpc.ChannelAcceptResponse{
				Accept:        true,
				PendingChanId: protoMsg.PendingChanId,
			}

			resMsg, err := lnrpc.RESTJsonMarshalOpts.Marshal(res)
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}
			err = conn.WriteMessage(websocket.TextMessage, resMsg)
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}
			ht.Logf("Finish writing message %s", resMsg)

			// Also send the message on our message channel to make
			// sure we count it as successful.
			msgChan <- res

			// Are we done or should there be more messages?
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// Before we start opening channels, make sure the two nodes are
	// connected.
	bob := ht.NewNodeWithCoins("Bob", nil)
	ht.EnsureConnected(alice, bob)

	assertMsgReceived := func() {
		select {
		case <-msgChan:
		case err := <-errChan:
			ht.Fatalf("Received error from WS: %v", err)

		case <-time.After(defaultTimeout):
			ht.Fatalf("Timeout before message was received")
		}
	}

	// Open 3 channels to make sure multiple requests and responses can be
	// sent over the web socket.
	ht.OpenChannel(bob, alice, lntest.OpenChannelParams{Amt: 500000})
	assertMsgReceived()

	ht.OpenChannel(bob, alice, lntest.OpenChannelParams{Amt: 500000})
	assertMsgReceived()

	ht.OpenChannel(bob, alice, lntest.OpenChannelParams{Amt: 500000})
	assertMsgReceived()
}

func wsTestPingPongTimeout(ht *lntest.HarnessTest) {
	initialRequest := &lnrpc.InvoiceSubscription{
		AddIndex: 1, SettleIndex: 1,
	}
	url := "/v1/invoices/subscribe"

	// This time we send the macaroon in the special header
	// Sec-Websocket-Protocol which is the only header field available to
	// browsers when opening a WebSocket.
	alice := ht.NewNode("Alice", nil)
	mac, err := alice.ReadMacaroon(
		alice.Cfg.AdminMacPath, defaultTimeout,
	)
	require.NoError(ht, err, "read admin mac")
	macBytes, err := mac.MarshalBinary()
	require.NoError(ht, err, "marshal admin mac")

	customHeader := make(http.Header)
	customHeader.Set(lnrpc.HeaderWebSocketProtocol, fmt.Sprintf(
		"Grpc-Metadata-Macaroon+%s", hex.EncodeToString(macBytes),
	))
	conn, err := openWebSocket(
		alice, url, "GET", initialRequest, customHeader,
	)
	require.Nil(ht, err, "websocket")
	defer func() {
		err := conn.WriteMessage(websocket.CloseMessage, closeMsg)
		_ = conn.Close()
		require.NoError(ht, err)
	}()

	// We want to be able to read invoices for a long time, making sure we
	// can continue to read even after we've gone through several ping/pong
	// cycles.
	invoices := make(chan *lnrpc.Invoice, 1)
	errChan := make(chan error)
	done := make(chan struct{})
	timeout := time.After(defaultTimeout)

	defer close(done)
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}

			// The chunked/streamed responses come wrapped in either
			// a {"result":{}} or {"error":{}} wrapper which we'll
			// get rid of here.
			msgStr := string(msg)
			if !strings.Contains(msgStr, "\"result\":") {
				select {
				case errChan <- fmt.Errorf("invalid msg: %s",
					msgStr):
				case <-done:
				}
				return
			}
			msgStr = resultPattern.ReplaceAllString(msgStr, "${1}")

			// Make sure we can parse the unwrapped message into the
			// expected proto message.
			protoMsg := &lnrpc.Invoice{}
			err = lnrpc.RESTJsonUnmarshalOpts.Unmarshal(
				[]byte(msgStr), protoMsg,
			)
			if err != nil {
				select {
				case errChan <- err:
				case <-done:
				}
				return
			}

			invoices <- protoMsg

			// Make sure we exit the loop once we've sent through
			// all expected test messages.
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// The SubscribeInvoices call returns immediately after the gRPC/REST
	// connection is established. But it can happen that the goroutine in
	// lnd that actually registers the subscriber in the invoice backend
	// didn't get any CPU time just yet. So we can run into the situation
	// where we add our first invoice _before_ the subscription client is
	// registered. If that happens, we'll never get notified about the
	// invoice in question. So all we really can do is wait a bit here to
	// make sure the subscription is registered correctly.
	time.Sleep(500 * time.Millisecond)

	// Let's create five invoices and wait for them to arrive. We'll wait
	// for at least one ping/pong cycle between each invoice.
	const numInvoices = 5
	const value = 123
	const memo = "websocket"
	for i := 0; i < numInvoices; i++ {
		invoice := &lnrpc.Invoice{
			Value: value,
			Memo:  memo,
		}
		alice.RPC.AddInvoice(invoice)

		select {
		case streamMsg := <-invoices:
			require.Equal(ht, int64(value), streamMsg.Value)
			require.Equal(ht, memo, streamMsg.Memo)

		case err := <-errChan:
			require.Fail(ht, "Error reading invoice: %v", err)

		case <-timeout:
			require.Fail(ht, "No invoice msg received in time")
		}

		// Let's wait for at least a whole ping/pong cycle to happen, so
		// we can be sure the read/write deadlines are set correctly.
		// We double the pong wait just to add some extra margin.
		time.Sleep(pingInterval + 2*pongWait)
	}
}

// invokeGET calls the given URL with the GET method and appropriate macaroon
// header fields then tries to unmarshal the response into the given response
// proto message.
func invokeGET(node *node.HarnessNode, url string, resp proto.Message) error {
	_, rawResp, err := makeRequest(node, url, "GET", nil, nil)
	if err != nil {
		return err
	}

	return lnrpc.RESTJsonUnmarshalOpts.Unmarshal(rawResp, resp)
}

// invokePOST calls the given URL with the POST method, request body and
// appropriate macaroon header fields then tries to unmarshal the response into
// the given response proto message.
func invokePOST(node *node.HarnessNode, url string, req,
	resp proto.Message) error {

	// Marshal the request to JSON using the REST marshaler to get correct
	// field names.
	reqBytes, err := lnrpc.RESTJsonMarshalOpts.Marshal(req)
	if err != nil {
		return err
	}

	_, rawResp, err := makeRequest(
		node, url, "POST", bytes.NewReader(reqBytes), nil,
	)
	if err != nil {
		return err
	}

	return lnrpc.RESTJsonUnmarshalOpts.Unmarshal(rawResp, resp)
}

// makeRequest calls the given URL with the given method, request body and
// appropriate macaroon header fields and returns the raw response body.
func makeRequest(node *node.HarnessNode, url, method string,
	request io.Reader, additionalHeaders http.Header) (http.Header, []byte,
	error) {

	// Assemble the full URL from the node's listening address then create
	// the request so we can set the macaroon on it.
	fullURL := fmt.Sprintf("https://%s%s", node.Cfg.RESTAddr(), url)
	req, err := http.NewRequest(method, fullURL, request)
	if err != nil {
		return nil, nil, err
	}
	if err := addAdminMacaroon(node, req.Header); err != nil {
		return nil, nil, err
	}
	for key, values := range additionalHeaders {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	// Do the actual call with the completed request object now.
	resp, err := restClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	return resp.Header, data, err
}

// openWebSocket opens a new WebSocket connection to the given URL with the
// appropriate macaroon headers and sends the request message over the socket.
func openWebSocket(node *node.HarnessNode, url, method string,
	req proto.Message, customHeader http.Header) (*websocket.Conn, error) {

	// Prepare our macaroon headers and assemble the full URL from the
	// node's listening address. WebSockets always work over GET so we need
	// to append the target request method as a query parameter.
	header := customHeader
	if header == nil {
		header = make(http.Header)
		if err := addAdminMacaroon(node, header); err != nil {
			return nil, err
		}
	}
	fullURL := fmt.Sprintf(
		"wss://%s%s?method=%s", node.Cfg.RESTAddr(), url, method,
	)
	conn, resp, err := webSocketDialer.Dial(fullURL, header)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	// Send the given request message as the first message on the socket.
	reqMsg, err := lnrpc.RESTJsonMarshalOpts.Marshal(req)
	if err != nil {
		return nil, err
	}
	err = conn.WriteMessage(websocket.TextMessage, reqMsg)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// addAdminMacaroon reads the admin macaroon from the node and appends it to
// the HTTP header fields.
func addAdminMacaroon(node *node.HarnessNode, header http.Header) error {
	mac, err := node.ReadMacaroon(node.Cfg.AdminMacPath, defaultTimeout)
	if err != nil {
		return err
	}
	macBytes, err := mac.MarshalBinary()
	if err != nil {
		return err
	}

	header.Set("Grpc-Metadata-Macaroon", hex.EncodeToString(macBytes))

	return nil
}
