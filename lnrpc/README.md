lnrpc
=====

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)](https://travis-ci.org/lightningnetwork/lnd) 
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

The list of defined RPC's on the main service are the following (with a brief
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
  * SubscribeTransactions
     * Returns a stream which sends async notifications each time a transaction
       is created or one is received that pays to us.
  * SendMany
     * Allows the caller to create a transaction with an arbitrary fan-out
       (many outputs).
  * NewAddress
     * Returns a new address, the following address types are supported:
       pay-to-public-key-hash (p2pkh), pay-to-witness-key-hash (p2wkh), and
       nested-pay-to-witness-key-hash (np2wkh).
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
  * PendingChannels
     * List the number of pending (not fully confirmed) channels.
  * ListChannels
     * List all active channels the daemon manages.
  * OpenChannel
     * Attempts to open a channel to a target peer with a specific amount and
       push amount.
  * CloseChannel
     * Attempts to close a target channel. A channel can either be closed
       cooperatively if the channel peer is online, or using a "force" close to
       broadcast the latest channel state.
  * SendPayment
     * Send a payment over Lightning to a target peer.
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
  * ListPayments
     * List all outgoing Lightning payments the daemon has made.
  * DescribeGraph
     * Returns a description of the known channel graph from the PoV of the
       node.
  * GetChanInfo
     * Returns information for a specific channel identified by channel ID.
  * GetNodeInfo
     * Returns information for a particular node identified by its identity
       public key.
  * QueryRoute
     * Queries for a possible route to a target peer which can carry a certain
       amount of payment.
  * GetNetworkInfo
     * Returns some network level statistics.

## Installation and Updating

```bash
$ go get -u github.com/lightningnetwork/lnd/lnrpc
```
