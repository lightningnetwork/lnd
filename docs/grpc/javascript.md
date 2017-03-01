# How to write a simple lnd client in Javascript using nodejs?

First, you need to initialize a simple nodejs project:

```
npm init (or npm init -f if you want to use the default values without prompt)
```

Then you need to install the Javascript grpc library dependency:

```
npm install grpc --save
```

You also need to copy the `lnd` `rpc.proto` file in your project directory (or at least somewhere reachable by your Javascript code).

The `rpc.proto` file is located in the `lnrpc` directory of the `lnd` sources.

I had to comment the following line in the `rpc.proto` file otherwise `grpc` would crash:

```
//import "google/api/annotations.proto";
```

Let's now write some Javascript code that simply displays the getinfo command result on the console:

```
var grpc = require('grpc');

var lnrpcDescriptor = grpc.load("rpc.proto");

var lnrpc = lnrpcDescriptor.lnrpc;

var lightning = new lnrpc.Lightning('localhost:10009', grpc.credentials.createInsecure());

lightning.getInfo({}, function(err, response) {
	console.log('GetInfo:', response);
});

```

You just have to lauch your newly created Javascript file `getinfo.js` using `nodejs`:

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

Enjoy!