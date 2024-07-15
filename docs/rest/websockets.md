# WebSockets with `lnd`'s REST API

This document describes how streaming response REST calls can be used correctly
by making use of the WebSocket API.

As an example, we are going to write a simple JavaScript program that subscribes
to `lnd`'s
[block notification RPC](https://api.lightning.community/#v2-chainnotifier-register-blocks).

The WebSocket will be kept open as long as `lnd` runs and JavaScript program
isn't stopped.

## Browser environment

When using WebSockets in a browser, there are certain security limitations of
what header fields are allowed to be sent. Therefore, the macaroon cannot just
be added as a `Grpc-Metadata-Macaroon` header field as it would work with normal
REST calls. The browser will just ignore that header field and not send it.

Instead, we have added a workaround in `lnd`'s WebSocket proxy that allows
sending the macaroon as a WebSocket "protocol":

```javascript
const host = 'localhost:8080'; // The default REST port of lnd, can be overwritten with --restlisten=ip:port
const macaroon = '0201036c6e6402eb01030a10625e7e60fd00f5a6f9cd53f33fc82a...'; // The hex encoded macaroon to send
const initialRequest = { // The initial request to send (see API docs for each RPC).
    hash: "xlkMdV382uNPskw6eEjDGFMQHxHNnZZgL47aVDSwiRQ=", // Just some example to show that all `byte` fields always have to be base64 encoded in the REST API.
    height: 144,
}

// The protocol is our workaround for sending the macaroon because custom header
// fields aren't allowed to be sent by the browser when opening a WebSocket.
const protocolString = 'Grpc-Metadata-Macaroon+' + macaroon;

// Let's now connect the web socket. Notice that all WebSocket open calls are
// always GET requests. If the RPC expects a call to be POST or DELETE (see API
// docs to find out), the query parameter "method" can be set to overwrite.
const wsUrl = 'wss://' + host + '/v2/chainnotifier/register/blocks?method=POST';
let ws = new WebSocket(wsUrl, protocolString);
ws.onopen = function (event) {
    // After the WS connection is establishes, lnd expects the client to send the
    // initial message. If an RPC doesn't have any request parameters, an empty
    // JSON object has to be sent as a string, for example: ws.send('{}')
    ws.send(JSON.stringify(initialRequest));
}
ws.onmessage = function (event) {
    // We received a new message.
    console.log(event);

    // The data we're really interested in is in data and is always a string
    // that needs to be parsed as JSON and always contains a "result" field:
    console.log("Payload: ");
    console.log(JSON.parse(event.data).result);
}
ws.onerror = function (event) {
    // An error occurred, let's log it to the console.
    console.log(event);
}
```

## Node.js environment

With Node.js it is a bit easier to use the streaming response APIs because we
can set the macaroon header field directly. This is the example from the API
docs:

```javascript
// --------------------------
// Example with websockets:
// --------------------------
const WebSocket = require('ws');
const fs = require('fs');
const macaroon = fs.readFileSync('LND_DIR/data/chain/bitcoin/simnet/admin.macaroon').toString('hex');
let ws = new WebSocket('wss://localhost:8080/v2/chainnotifier/register/blocks?method=POST', {
  // Work-around for self-signed certificates.
  rejectUnauthorized: false,
  headers: {
    'Grpc-Metadata-Macaroon': macaroon,
  },
});
let requestBody = { 
  hash: "<byte>",
  height: "<int64>",
}
ws.on('open', function() {
    ws.send(JSON.stringify(requestBody));
});
ws.on('error', function(err) {
    console.log('Error: ' + err);
});
ws.on('message', function(body) {
    console.log(body);
});
// Console output (repeated for every message in the stream):
//  { 
//      "hash": <byte>, 
//      "height": <int64>, 
//  }
```

## Request-streaming RPCs

Starting with `lnd v0.13.0-beta` all RPCs can be used through REST, even those
that are fully bidirectional (e.g. the client can also send multiple request
messages to the stream).

**Example**:

As an example we show how one can use the bidirectional channel acceptor RPC.
Through that RPC each incoming channel open request (another peer opening a
channel to our node) will be passed in for inspection. We can decide
programmatically whether to accept or reject the channel.

```javascript
// --------------------------
// Example with websockets:
// --------------------------
const WebSocket = require('ws');
const fs = require('fs');
const macaroon = fs.readFileSync('LND_DIR/data/chain/bitcoin/simnet/admin.macaroon').toString('hex');
let ws = new WebSocket('wss://localhost:8080/v1/channels/acceptor?method=POST', {
  // Work-around for self-signed certificates.
  rejectUnauthorized: false,
  headers: {
    'Grpc-Metadata-Macaroon': macaroon,
  },
});
ws.on('open', function() {
    // We always _need_ to send an initial message to kickstart the request.
    // This empty message will be ignored by the channel acceptor though, this
    // is just for telling the grpc-gateway library that it can forward the
    // request to the gRPC interface now. If this were an RPC where the client
    // always sends the first message (for example the streaming payment RPC
    // /v1/channels/transaction-stream), we'd simply send the first "real"
    // message here when needed.
    ws.send('{}');
});
ws.on('error', function(err) {
    console.log('Error: ' + err);
});
ws.on('ping', function ping(event) {
   console.log('Received ping from server: ' + JSON.stringify(event)); 
});
ws.on('message', function incoming(event) {
    console.log('New channel accept message: ' + event);
    const result = JSON.parse(event).result;
    
    // Accept the channel after inspecting it.
    ws.send(JSON.stringify({accept: true, pending_chan_id: result.pending_chan_id}));
});
```
