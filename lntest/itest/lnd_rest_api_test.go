// +build rpctest

package itest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lntest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	insecureTransport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	restClient = &http.Client{
		Transport: insecureTransport,
	}
	jsonMarshaler = &jsonpb.Marshaler{
		EmitDefaults: true,
		OrigName:     true,
		Indent:       "    ",
	}
	urlEnc          = base64.URLEncoding
	webSocketDialer = &websocket.Dialer{
		HandshakeTimeout: 45 * time.Second,
		TLSClientConfig:  insecureTransport.TLSClientConfig,
	}
	resultPattern = regexp.MustCompile("{\"result\":(.*)}")
)

// testRestApi tests that the most important features of the REST API work
// correctly.
func testRestApi(net *lntest.NetworkHarness, ht *harnessTest) {
	testCases := []struct {
		name string
		run  func(*testing.T, *lntest.HarnessNode, *lntest.HarnessNode)
	}{{
		name: "simple GET",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Check that the parsing into the response proto
			// message works.
			resp := &lnrpc.GetInfoResponse{}
			err := invokeGET(a, "/v1/getinfo", resp)
			require.Nil(t, err, "getinfo")
			assert.Equal(t, "#3399ff", resp.Color, "node color")

			// Make sure we get the correct field names (snake
			// case).
			_, resp2, err := makeRequest(
				a, "/v1/getinfo", "GET", nil, nil,
			)
			require.Nil(t, err, "getinfo")
			assert.Contains(
				t, string(resp2), "best_header_timestamp",
				"getinfo",
			)
		},
	}, {
		name: "simple POST and GET with query param",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Add an invoice, testing POST in the process.
			req := &lnrpc.Invoice{Value: 1234}
			resp := &lnrpc.AddInvoiceResponse{}
			err := invokePOST(a, "/v1/invoices", req, resp)
			require.Nil(t, err, "add invoice")
			assert.Equal(t, 32, len(resp.RHash), "invoice rhash")

			// Make sure we can call a GET endpoint with a hex
			// encoded URL part.
			url := fmt.Sprintf("/v1/invoice/%x", resp.RHash)
			resp2 := &lnrpc.Invoice{}
			err = invokeGET(a, url, resp2)
			require.Nil(t, err, "query invoice")
			assert.Equal(t, int64(1234), resp2.Value, "invoice amt")
		},
	}, {
		name: "GET with base64 encoded byte slice in path",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			url := "/v2/router/mc/probability/%s/%s/%d"
			url = fmt.Sprintf(
				url, urlEnc.EncodeToString(a.PubKey[:]),
				urlEnc.EncodeToString(b.PubKey[:]), 1234,
			)
			resp := &routerrpc.QueryProbabilityResponse{}
			err := invokeGET(a, url, resp)
			require.Nil(t, err, "query probability")
			assert.Greater(t, resp.Probability, 0.5, "probability")
		},
	}, {
		name: "GET with map type query param",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Get a new wallet address from Alice.
			ctxb := context.Background()
			newAddrReq := &lnrpc.NewAddressRequest{
				Type: lnrpc.AddressType_WITNESS_PUBKEY_HASH,
			}
			addrRes, err := a.NewAddress(ctxb, newAddrReq)
			require.Nil(t, err, "get address")

			// Create the full URL with the map query param.
			url := "/v1/transactions/fee?target_conf=%d&" +
				"AddrToAmount[%s]=%d"
			url = fmt.Sprintf(url, 2, addrRes.Address, 50000)
			resp := &lnrpc.EstimateFeeResponse{}
			err = invokeGET(a, url, resp)
			require.Nil(t, err, "estimate fee")
			assert.Greater(t, resp.FeeSat, int64(253), "fee")
		},
	}, {
		name: "sub RPC servers REST support",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Query autopilot status.
			res1 := &autopilotrpc.StatusResponse{}
			err := invokeGET(a, "/v2/autopilot/status", res1)
			require.Nil(t, err, "autopilot status")
			assert.Equal(t, false, res1.Active, "autopilot status")

			// Query the version RPC.
			res2 := &verrpc.Version{}
			err = invokeGET(a, "/v2/versioner/version", res2)
			require.Nil(t, err, "version")
			assert.Greater(
				t, res2.AppMinor, uint32(0), "lnd minor version",
			)

			// Request a new external address from the wallet kit.
			req1 := &walletrpc.AddrRequest{}
			res3 := &walletrpc.AddrResponse{}
			err = invokePOST(
				a, "/v2/wallet/address/next", req1, res3,
			)
			require.Nil(t, err, "address")
			assert.NotEmpty(t, res3.Addr, "address")
		},
	}, {
		name: "CORS headers",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Alice allows all origins. Make sure we get the same
			// value back in the CORS header that we send in the
			// Origin header.
			reqHeaders := make(http.Header)
			reqHeaders.Add("Origin", "https://foo.bar:9999")
			resHeaders, body, err := makeRequest(
				a, "/v1/getinfo", "OPTIONS", nil, reqHeaders,
			)
			require.Nil(t, err, "getinfo")
			assert.Equal(
				t, "https://foo.bar:9999",
				resHeaders.Get("Access-Control-Allow-Origin"),
				"CORS header",
			)
			assert.Equal(t, 0, len(body))

			// Make sure that we don't get a value set for Bob which
			// doesn't allow any CORS origin.
			resHeaders, body, err = makeRequest(
				b, "/v1/getinfo", "OPTIONS", nil, reqHeaders,
			)
			require.Nil(t, err, "getinfo")
			assert.Equal(
				t, "",
				resHeaders.Get("Access-Control-Allow-Origin"),
				"CORS header",
			)
			assert.Equal(t, 0, len(body))
		},
	}, {
		name: "websocket subscription",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Find out the current best block so we can subscribe
			// to the next one.
			hash, height, err := net.Miner.Node.GetBestBlock()
			require.Nil(t, err, "get best block")

			// Create a new subscription to get block epoch events.
			req := &chainrpc.BlockEpoch{
				Hash:   hash.CloneBytes(),
				Height: uint32(height),
			}
			url := "/v2/chainnotifier/register/blocks"
			c, err := openWebSocket(a, url, "POST", req, nil)
			require.Nil(t, err, "websocket")
			defer func() {
				_ = c.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(
						websocket.CloseNormalClosure,
						"done",
					),
				)
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

				// The chunked/streamed responses come wrapped
				// in either a {"result":{}} or {"error":{}}
				// wrapper which we'll get rid of here.
				msgStr := string(msg)
				if !strings.Contains(msgStr, "\"result\":") {
					errChan <- fmt.Errorf("invalid msg: %s",
						msgStr)
					return
				}
				msgStr = resultPattern.ReplaceAllString(
					msgStr, "${1}",
				)

				// Make sure we can parse the unwrapped message
				// into the expected proto message.
				protoMsg := &chainrpc.BlockEpoch{}
				err = jsonpb.UnmarshalString(
					msgStr, protoMsg,
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
			blockHashes, err := net.Miner.Node.Generate(1)
			require.Nil(t, err, "generate blocks")
			assert.Equal(t, 1, len(blockHashes), "num blocks")
			select {
			case msg := <-msgChan:
				assert.Equal(
					t, blockHashes[0].CloneBytes(),
					msg.Hash, "block hash",
				)

			case err := <-errChan:
				t.Fatalf("Received error from WS: %v", err)

			case <-timeout:
				t.Fatalf("Timeout before message was received")
			}
		},
	}, {
		name: "websocket subscription with macaroon in protocol",
		run: func(t *testing.T, a, b *lntest.HarnessNode) {
			// Find out the current best block so we can subscribe
			// to the next one.
			hash, height, err := net.Miner.Node.GetBestBlock()
			require.Nil(t, err, "get best block")

			// Create a new subscription to get block epoch events.
			req := &chainrpc.BlockEpoch{
				Hash:   hash.CloneBytes(),
				Height: uint32(height),
			}
			url := "/v2/chainnotifier/register/blocks"

			// This time we send the macaroon in the special header
			// Sec-Websocket-Protocol which is the only header field
			// available to browsers when opening a WebSocket.
			mac, err := a.ReadMacaroon(
				a.AdminMacPath(), defaultTimeout,
			)
			require.NoError(t, err, "read admin mac")
			macBytes, err := mac.MarshalBinary()
			require.NoError(t, err, "marshal admin mac")

			customHeader := make(http.Header)
			customHeader.Set(
				lnrpc.HeaderWebSocketProtocol, fmt.Sprintf(
					"Grpc-Metadata-Macaroon+%s",
					hex.EncodeToString(macBytes),
				),
			)
			c, err := openWebSocket(
				a, url, "POST", req, customHeader,
			)
			require.Nil(t, err, "websocket")
			defer func() {
				_ = c.WriteMessage(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(
						websocket.CloseNormalClosure,
						"done",
					),
				)
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

				// The chunked/streamed responses come wrapped
				// in either a {"result":{}} or {"error":{}}
				// wrapper which we'll get rid of here.
				msgStr := string(msg)
				if !strings.Contains(msgStr, "\"result\":") {
					errChan <- fmt.Errorf("invalid msg: %s",
						msgStr)
					return
				}
				msgStr = resultPattern.ReplaceAllString(
					msgStr, "${1}",
				)

				// Make sure we can parse the unwrapped message
				// into the expected proto message.
				protoMsg := &chainrpc.BlockEpoch{}
				err = jsonpb.UnmarshalString(
					msgStr, protoMsg,
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
			blockHashes, err := net.Miner.Node.Generate(1)
			require.Nil(t, err, "generate blocks")
			assert.Equal(t, 1, len(blockHashes), "num blocks")
			select {
			case msg := <-msgChan:
				assert.Equal(
					t, blockHashes[0].CloneBytes(),
					msg.Hash, "block hash",
				)

			case err := <-errChan:
				t.Fatalf("Received error from WS: %v", err)

			case <-timeout:
				t.Fatalf("Timeout before message was received")
			}
		},
	}}

	// Make sure Alice allows all CORS origins. Bob will keep the default.
	net.Alice.Cfg.ExtraArgs = append(
		net.Alice.Cfg.ExtraArgs, "--restcors=\"*\"",
	)
	err := net.RestartNode(net.Alice, nil)
	if err != nil {
		ht.t.Fatalf("Could not restart Alice to set CORS config: %v",
			err)
	}

	for _, tc := range testCases {
		ht.t.Run(tc.name, func(t *testing.T) {
			tc.run(t, net.Alice, net.Bob)
		})
	}
}

// invokeGET calls the given URL with the GET method and appropriate macaroon
// header fields then tries to unmarshal the response into the given response
// proto message.
func invokeGET(node *lntest.HarnessNode, url string, resp proto.Message) error {
	_, rawResp, err := makeRequest(node, url, "GET", nil, nil)
	if err != nil {
		return err
	}

	return jsonpb.Unmarshal(bytes.NewReader(rawResp), resp)
}

// invokePOST calls the given URL with the POST method, request body and
// appropriate macaroon header fields then tries to unmarshal the response into
// the given response proto message.
func invokePOST(node *lntest.HarnessNode, url string, req,
	resp proto.Message) error {

	// Marshal the request to JSON using the jsonpb marshaler to get correct
	// field names.
	var buf bytes.Buffer
	if err := jsonMarshaler.Marshal(&buf, req); err != nil {
		return err
	}

	_, rawResp, err := makeRequest(node, url, "POST", &buf, nil)
	if err != nil {
		return err
	}

	return jsonpb.Unmarshal(bytes.NewReader(rawResp), resp)
}

// makeRequest calls the given URL with the given method, request body and
// appropriate macaroon header fields and returns the raw response body.
func makeRequest(node *lntest.HarnessNode, url, method string,
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

	data, err := ioutil.ReadAll(resp.Body)
	return resp.Header, data, err
}

// openWebSocket opens a new WebSocket connection to the given URL with the
// appropriate macaroon headers and sends the request message over the socket.
func openWebSocket(node *lntest.HarnessNode, url, method string,
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
	conn, _, err := webSocketDialer.Dial(fullURL, header)
	if err != nil {
		return nil, err
	}

	// Send the given request message as the first message on the socket.
	reqMsg, err := jsonMarshaler.MarshalToString(req)
	if err != nil {
		return nil, err
	}
	err = conn.WriteMessage(websocket.TextMessage, []byte(reqMsg))
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// addAdminMacaroon reads the admin macaroon from the node and appends it to
// the HTTP header fields.
func addAdminMacaroon(node *lntest.HarnessNode, header http.Header) error {
	mac, err := node.ReadMacaroon(node.AdminMacPath(), defaultTimeout)
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
