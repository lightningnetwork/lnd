# lnd - lightning network daemon

This repo is preliminary work on a lightning network peer to peer node and wallet.

It currently is being designed for testnet-L, a test network where *all* txids are normalized.  This fixes malleability but isn't something that can be cleanly done to the existing bitcoin network.

It is not yet functional, but we hope to have a proof of concept on testnet-L soon.

Here's a quick break down of the current packages within the repo: 

## chainntfs

A package centered around a generic interface for receiving transaction/confirmation based notifications concerning the blockchain. Such notifications are required in order for pending payment channels to be notified once the funding transaction gains a specified number of confirmations, and in order to catch a counter-party attempting a non-cooperative close using a past commitment transaction to steal funds. 

At the moment, it only has a single concrete implementation: using btcd's websockets notifications. However, more implementations of the interface are planned, such as electrum, polling via JSON-RPC, Bitcoin Core's ZeroMQ notifications, and more. 

## channeldb 

lnd's primary datastore. It uses a generic interface defined in [walletdb](https://godoc.org/github.com/btcsuite/btcwallet/walletdb) allowing for usage of any sotrage backend which adheres to the interface. The current concrete implementation used is driven by [bolt](https://github.com/boltdb/bolt). `channeldb` is responsible for storing state such as meta-data concerning the current open channels, closed channels, past routes we used, fee schedules within the network, and information about remote peers (ID, uptime, reputation, etc). 

## cmd / lncli
A command line to query and control a running lnd.  Similar to bitcoin-cli

## elkrem
Library to send and receive a tree structure of hashes which can be sequentially revealed.  If you want to send N secrets, you only need to send N secrets (no penalty there) but the receiver only needs to store log2(N) hashes, and can quickly compute any previous secret from those.

This is useful for the hashed secrets in LN payment channels.

## lndc
Library for authenticated encrypted communication between LN nodes.  It uses chacha20_poly1305 for the symmetric cipher, and the secp256k1 curve used in bitcoin for public keys.  No signing is used, only two ECDH calculations: first with ephemeral key pairs and second with persistent identifying public keys.

## lnrpc 

lnd's RPC interface. Currently [gRPC](http://www.grpc.io/), a high-performance RPC framework is used. gRPC provides features such as a stub-based client interface in several languages, streaming RPCs, payload agnostic request/response handling, and more. In addition to gRPC, lnd will also offer a basic REST-based http RPC interface. For now, connections are not encrypted, or authenticated. For authentication, [macaroons](http://research.google.com/pubs/pub41892.html) will likely be integrated due to their flexbility, and expresivness. 

## lnstate

An in-progress implementation of a payment channel state machine, which will allow for simultaneous high-volume, non-blocking, bi-directional payments. Once finalized, the primary bottleneck for payment throughput will be the hardware of a lnd node. A reasonably high-specced computer should be able to conduct transactions as Visa-scale. 

## lnwallet 

An application specific, yet general Bitcoin wallet which understands all the fancy script, and transaction formats needed to transact on the Lightning Network. The interface, and interaction with the core wallet logic has been designed independent of any peer-to-peer communication. The goal is to make lnwallet self-contained, and easily embeddable within future projects interacting with the Lightning Network. The current state machine for channel updates is blocking, only allowing one pending update at a time. This will soon be replaced in favor of the highly concurrent, non-blocking state machine within `lnstate`. 

## lnwire 

The current specification, and implementation of lnd's peer-to-peer wire protocol. This library handles serialization, and deserialization of lnd's messages. The primary API is a simple, message agnostic interface, allowing for a high degree of flexibility across both the implementation, and caller. 

## shachain

An implementation of Rusty Russel's [64-dimensional shachain](https://github.com/rustyrussell/ccan/blob/master/ccan/crypto/shachain/design.txt).

## uspv
Wallet library to connect to bitcoin nodes and build a local SPV and wallet transaction state.
