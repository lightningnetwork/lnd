lnrpc
=====

[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/lightningnetwork/lnd/lnrpc)

This lnrpc package implements both a client and server for `lnd`s RPC system
which is based off of the high-performance cross-platform
[gRPC](http://www.grpc.io/) RPC framework. By default, only the Go
client+server libraries are compiled within the package. In order to compile
the client side libraries for other supported languages, the `protoc` tool will
need to be used to generate the compiled protos for a specific language.

The following languages are supported as clients to `lnrpc`: C++, Go, Node.js,
Java, Ruby, Android Java, PHP, Python, C#, Objective-C.

## Service: Lightning

The list of defined RPCs on the service `Lightning` are the following (with a brief
description):

  * WalletBalance
     * Returns the wallet's current confirmed balance in BTC.
  * ChannelBalance
     * Returns the daemons' available aggregate channel balance in BTC.
  * GetTransactions
     * Returns a list of on-chain transactions that pay to or are spends from
       `lnd`.
  * SendCoins
     * Sends an amount of satoshis to a specific address.
  * ListUnspent
     * Lists available utxos within a range of confirmations.
  * SubscribeTransactions
     * Returns a stream which sends async notifications each time a transaction
       is created or one is received that pays to us.
  * SendMany
     * Allows the caller to create a transaction with an arbitrary fan-out
       (many outputs).
  * NewAddress
     * Returns a new address, the following address types are supported:
       pay-to-witness-key-hash (p2wkh) and nested-pay-to-witness-key-hash
       (np2wkh).
  * SignMessage
     * Signs a message with the node's identity key and returns a
       zbase32 encoded signature.
  * VerifyMessage
     * Verifies a signature signed by another node on a message. The other node
       must be an active node in the channel database.
  * ConnectPeer
     * Connects to a peer identified by a public key and host.
  * DisconnectPeer
     * Disconnects a peer identified by a public key.
  * ListPeers
     * Lists all available connected peers.
  * GetInfo
     * Returns basic data concerning the daemon.
  * GetRecoveryInfo
     * Returns information about recovery process.
  * PendingChannels
     * List the number of pending (not fully confirmed) channels.
  * ListChannels
     * List all active channels the daemon manages.
  * OpenChannelSync
     * OpenChannelSync is a synchronous version of the OpenChannel RPC call.
  * OpenChannel
     * Attempts to open a channel to a target peer with a specific amount and
       push amount.
  * CloseChannel
     * Attempts to close a target channel. A channel can either be closed
       cooperatively if the channel peer is online, or using a "force" close to
       broadcast the latest channel state.
  * SendPayment
     * Send a payment over Lightning to a target peer.
  * SendPaymentSync
     * SendPaymentSync is the synchronous non-streaming version of SendPayment.
  * SendToRoute
    * Send a payment over Lightning to a target peer through a route explicitly
      defined by the user.
  * SendToRouteSync
    * SendToRouteSync is the synchronous non-streaming version of SendToRoute.
  * AddInvoice
     * Adds an invoice to the daemon. Invoices are automatically settled once
       seen as an incoming HTLC.
  * ListInvoices
     * Lists all stored invoices.
  * LookupInvoice
     * Attempts to look up an invoice by payment hash (r-hash).
  * SubscribeInvoices
     * Creates a uni-directional stream which receives async notifications as
       the daemon settles invoices
  * DecodePayReq
     * Decode a payment request, returning a full description of the conditions
       encoded within the payment request.
  * ListPayments
     * List all outgoing Lightning payments the daemon has made.
  * DeleteAllPayments
     * Deletes all outgoing payments from DB.
  * DescribeGraph
     * Returns a description of the known channel graph from the PoV of the
       node.
  * GetChanInfo
     * Returns information for a specific channel identified by channel ID.
  * GetNodeInfo
     * Returns information for a particular node identified by its identity
       public key.
  * QueryRoutes
     * Queries for a possible route to a target peer which can carry a certain
       amount of payment.
  * GetNetworkInfo
     * Returns some network level statistics.
  * StopDaemon
     * Sends a shutdown request to the interrupt handler, triggering a graceful
       shutdown of the daemon.
  * SubscribeChannelGraph
     * Creates a stream which receives async notifications upon any changes to the
       channel graph topology from the point of view of the responding node.
  * DebugLevel
     * Set logging verbosity of lnd programmatically
  * FeeReport
     * Allows the caller to obtain a report detailing the current fee schedule
       enforced by the node globally for each channel.
  * UpdateChannelPolicy
     * Allows the caller to update the fee schedule and channel policies for all channels
       globally, or a particular channel.
  * ForwardingHistory
     * ForwardingHistory allows the caller to query the htlcswitch for a
       record of all HTLCs forwarded.
  * BakeMacaroon
     * Bakes a new macaroon with the provided list of permissions and
       restrictions
  * ListMacaroonIDs
     * List all the macaroon root key IDs that are in use.
  * DeleteMacaroonID
     * Remove a specific macaroon root key ID from the database and invalidates
       all macaroons derived from the key with that ID. 

## Service: WalletUnlocker

The list of defined RPCs on the service `WalletUnlocker` are the following (with a brief
description):

  * CreateWallet
     * Set encryption password for the wallet database.
  * UnlockWallet
     * Provide a password to unlock the wallet database.

## Installation and Updating

```shell
$  go get -u github.com/lightningnetwork/lnd/lnrpc
```

## Generate protobuf definitions

To compile the `lnrpc/**/*.proto` files and generate the protobuf definitions,
you need to have [Docker](https://docs.docker.com/get-docker/) and `make`
installed.

Simply run `make rpc` to start the compilation process.

## Format .proto files

We use `clang-format` to make sure the `.proto` files are formatted correctly.

When running the `make rpc` command, the `.proto` files are also formatted. To
format the files without also compiling them, you can install the `clang-format`
formatter on Ubuntu by running `apt install clang-format` or on Mac by running
`brew install clang-format`.
The `make format` command should then produce the correct result.

Consult [this page](http://releases.llvm.org/download.html) to find binaries
for other operating systems or distributions.

## Makefile commands

The following commands are available with `make`:

* `rpc`: Compile and format all `.proto` files using Docker (calls
  `lnrpc/gen_protos_docker.sh`).
* `rpc-format`: Formats all `.proto` files according to our formatting rules.
  Requires `clang-format`, see previous chapter.
* `rpc-check`: Runs both previous commands and makes sure the git work tree is
  not dirty. This can be used to check that the `.proto` files are formatted
  and compiled properly.
