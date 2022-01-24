# Release Notes

## Remote signing

The [remote signing](../remote-signing.md) setup was simplified in that the
signing node now [does not need to be hooked up to its own chain
backend](https://github.com/lightningnetwork/lnd/pull/6006). A new mock chain
backend can be specified with `--bitcoin.node=nochainbackend`. That way a wallet
will be created and all signing RPCs work but the node will not look at any
chain data. It can therefore be fully offline except for a single incoming gRPC
connection from the watch-only node.

## Wallet

* A bug that prevented opening anchor-based channels from external wallets when
  the internal wallet was empty even though the transaction contained a
  sufficiently large output belonging to the internal wallet
  [was fixed](https://github.com/lightningnetwork/lnd/pull/5539).
  In other words, freshly-installed LND can now be initialized with multiple
  channels from an external (e.g. hardware) wallet *in a single transaction*.

## Database

* [Speed up graph cache loading on startup with
Postgres](https://github.com/lightningnetwork/lnd/pull/6111)

## Build System

* [Clean up Makefile by using go
  install](https://github.com/lightningnetwork/lnd/pull/6035).

* [Make etcd max message size
  configurable](https://github.com/lightningnetwork/lnd/pull/6049).

* [Export bitcoind port and other values for itests, useful for
  using itest harness outside of
  lnd](https://github.com/lightningnetwork/lnd/pull/6050).

* [Export `lntest` base node config so it can be re-used in LiT integration
  tests](https://github.com/lightningnetwork/lnd/pull/6139).

## Bug fixes

* [Return the nearest known fee rate when a given conf target cannot be found
  from Web API fee estimator.](https://github.com/lightningnetwork/lnd/pull/6062)

* [We now _always_ set a channel type if the other party signals the feature
  bit](https://github.com/lightningnetwork/lnd/pull/6075).

* [Add `--json` flag to
  `trackpayment`](https://github.com/lightningnetwork/lnd/pull/6060).

* [Clarify invalid config timeout
  constraints](https://github.com/lightningnetwork/lnd/pull/6073).

* [Fix memory corruption in Mission Control
  Store](https://github.com/lightningnetwork/lnd/pull/6068)
 
* [Ensure that the min relay fee is always clamped by our fee
  floor](https://github.com/lightningnetwork/lnd/pull/6076)

* [Clarify log message about not running within
  systemd](https://github.com/lightningnetwork/lnd/pull/6096)

* [Fix Postgres context cancellation](https://github.com/lightningnetwork/lnd/pull/6108)

* A conflict was found in connecting peers, where the peer bootstrapping
  process and persistent connection could compete connection for a peer that
  led to an already made connection being lost. [This is now fixed so that
  bootstrapping will always ignore the peers chosen by the persistent
  connection.](https://github.com/lightningnetwork/lnd/pull/6082)
  
* [Fix Postgres itests max connections](https://github.com/lightningnetwork/lnd/pull/6116)

* [Fix duplicate db connection close](https://github.com/lightningnetwork/lnd/pull/6140)

* [Fix a memory leak introduced by the new ping-header p2p enhancement](https://github.com/lightningnetwork/lnd/pull/6144)

* [Fix an issue that would prevent very old nodes from starting up due to lack of a historical channel bucket](https://github.com/lightningnetwork/lnd/pull/6159)


## RPC Server

* [ChanStatusFlags is now
  exposed](https://github.com/lightningnetwork/lnd/pull/5971) inside
  WaitingCloseResp from calling `PendingChannels`.

* [Fix missing label on streamed
  transactions](https://github.com/lightningnetwork/lnd/pull/5854).

* [Closing txid is now
  exposed](https://github.com/lightningnetwork/lnd/pull/6146) inside
  WaitingCloseResp from calling `PendingChannels`.

* [CustomCaveatCondition is now properly
  set](https://github.com/lightningnetwork/lnd/pull/6185) on
  `RPCMiddlewareRequest` messages.


## Routing

* [Enable forced update of MC pair
  history](https://github.com/lightningnetwork/lnd/pull/6180) by adding the `force`
  flag to the `XImportMissionControl` RPC call.

## Documentation

* [General improvements to the mobile documentation](https://github.com/lightningnetwork/lnd/pull/6181). 

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Andreas Schjønhaug
* Bjarne Magnussen
* Daniel McNally
* Elle Mouton
* Harsha Goli
* Joost Jager
* Martin Habovštiak
* Naveen Srinivasan
* Oliver Gugger
* Yong Yu
