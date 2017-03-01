# How to write a simple `lnd` client in Javascript using `node.js

First, you'll need to initialize a simple nodejs project:

```
npm init (or npm init -f if you want to use the default values without prompt)
```

Then you need to install the Javascript grpc library dependency:

```
npm install grpc --save
```

You also need to copy the `lnd` `rpc.proto` file in your project directory (or
at least somewhere reachable by your Javascript code).

The `rpc.proto` file is [located in the `lnrpc` directory of the `lnd`
sources](https://github.com/lightningnetwork/lnd/blob/master/lnrpc/rpc.proto).

In order to allow the auto code generated to compile the protos succucsfully,
you'll need to comment out the following line:
```
//import "google/api/annotations.proto";
```

Let's now write some Javascript code that simply displays the `getinfo` command
result on the console:

```js
var grpc = require('grpc');

var lnrpcDescriptor = grpc.load("rpc.proto");

var lnrpc = lnrpcDescriptor.lnrpc;

var lightning = new lnrpc.Lightning('localhost:10009', grpc.credentials.createInsecure());

lightning.getInfo({}, function(err, response) {
	console.log('GetInfo:', response);
});

```

You just have to lauch your newly created Javascript file `getinfo.js` using
`nodejs`:

```
node getinfo.js
```

You should get something like this in your console:

```
GetInfo: { identity_pubkey: '03c892e3f3f077ea1e381c081abb36491a2502bc43ed37ffb82e264224f325ff27',
  alias: '',
  num_pending_channels: 0,
  num_active_channels: 0,
  num_peers: 0,
  block_height: 1087612,
  block_hash: '000000000000024b2acc37958b15010057c6abc0a48f83c8dd67034bee2cb823',
  synced_to_chain: true,
  testnet: true }
```

With the above, you should have all the `lnd` related `gRPC` dependencies
installed locally in your local project. In order to get up to speed with
`protofbuf` usage from Javascript, see [this official `protobuf` reference for
Javascript](https://developers.google.com/protocol-buffers/docs/reference/javascript-generated).
Additionally, [this official gRPC
resource](http://www.grpc.io/docs/tutorials/basic/node.html) details how to
drive `gRPC` from `node.js` including the basics of making RPC calls, streaming
RPC's (bi-directional and uni-directional), etc.
