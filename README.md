# lnd - Lightning Network Daemon

This repo is preliminary work on a lightning network peer-to-peer node and wallet.

It's currently being tested on Segnet4, where malleability has been solved with segwit.

This version of Lnd is not yet fully-operational, but a proof of concept on Segnet4 will likely exist soon.  The projection is it will be operational before the necessary malleability soft-forks are into bitcoin mainnet (may be significantly before if there are any delays in mainnet malleability fix).

Don't try to port it to mainnet or an altcoin and use it today!  No really.  Lightning transactions will be fast, but for now, please wait just a little bit.

## Installation

* If necessary, install Go according to the installation instructions here: http://golang.org/doc/install. It is recommended to add `$GOPATH/bin` to your `PATH` at this point.
* Run the following command to obtain and install lnd, lncli, lnshell and all dependencies:

```
go get -u -v github.com/lightningnetwork/lnd/...
```

## Packages and Utilities

### chainntfs

A package centered around a generic interface for receiving transaction/confirmation based notifications concerning the blockchain. Such notifications are required in order for pending payment channels to be notified once the funding transaction gains a specified number of confirmations, and in order to catch a counter-party attempting a non-cooperative close using a past commitment transaction to steal funds.

At the moment, it only has a single concrete implementation: using btcd's websockets notifications. However, more implementations of the interface are planned, such as electrum, polling via JSON-RPC, Bitcoin Core's ZeroMQ notifications, and more.

### channeldb

lnd's primary datastore. It uses a generic interface defined in [walletdb](https://godoc.org/github.com/btcsuite/btcwallet/walletdb) allowing for usage of any storage backend which adheres to the interface. The current concrete implementation used is driven by [bolt](https://github.com/boltdb/bolt). `channeldb` is responsible for storing state such as meta-data concerning the current open channels, closed channels, past routes we used, fee schedules within the network, and information about remote peers (ID, uptime, reputation, etc).

### cmd / lncli
A command line to query and control a running lnd.  Similar to bitcoin-cli

### cmd / lnshell
Interactive shell for commands to direct the lnd node.  Will probably be merged into lnd soon as a command line option.

### elkrem
Library to send and receive a tree structure of hashes which can be sequentially revealed.  If you want to send N secrets, you only need to send N secrets (no penalty there) but the receiver only needs to store log2(N) hashes, and can quickly compute any previous secret from those.

This is useful for the hashed secrets in LN payment channels.

### lndc
Library for authenticated encrypted communication between LN nodes.  It uses chacha20_poly1305 for the symmetric cipher, and the secp256k1 curve used in bitcoin for public keys.  No signing is used, only two ECDH calculations: first with ephemeral key pairs and second with persistent identifying public keys.

### lnrpc

lnd's RPC interface. Currently [gRPC](http://www.grpc.io/), a high-performance RPC framework is used. gRPC provides features such as a stub-based client interface in several languages, streaming RPCs, payload agnostic request/response handling, and more. In addition to gRPC, lnd will also offer a basic REST-based http RPC interface. For now, connections are not encrypted, or authenticated. For authentication, [macaroons](http://research.google.com/pubs/pub41892.html) will likely be integrated due to their simplicity, flexibility, and expressiveness.

### lnstate
In-progress update which improves current implementation of channel state machine to allow for higher throughput.

### lnwallet

An application specific, yet general Bitcoin wallet which understands all the fancy script, and transaction formats needed to transact on the Lightning Network. The interface, and interaction with the core wallet logic has been designed independent of any peer-to-peer communication. The goal is to make lnwallet self-contained, and easily embeddable within future projects interacting with the Lightning Network. The current state machine for channel updates is blocking, only allowing one pending update at a time. This will soon be replaced in favor of the highly concurrent, non-blocking state machine within `lnstate`.

### lnwire
Library of messages for Lightning Network node to node communications.  Messages for opening and closing channels, as well as updating and revoking channel states are described here.

### shachain
An implementation of Rusty Russell's [64-dimensional shachain](https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt).

### uspv
Wallet library to connect to bitcoin nodes and build a local SPV and wallet transaction state.
