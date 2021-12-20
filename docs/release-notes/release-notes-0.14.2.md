# Release Notes

## Bug Fixes

* [Return the nearest known fee rate when a given conf target cannot be found
  from Web API fee estimator.](https://github.com/lightningnetwork/lnd/pull/6062)

* [We now _always_ set a channel type if the other party signals the feature
  bit](https://github.com/lightningnetwork/lnd/pull/6075).

## Wallet

* A bug that prevented opening anchor-based channels from external wallets when
  the internal wallet was empty even though the transaction contained a
  sufficiently large output belonging to the internal wallet
  [was fixed](https://github.com/lightningnetwork/lnd/pull/5539)
  In other words, freshly-installed LND can now be initailized with multiple
  channels from an external (e.g. hardware) wallet *in a single transaction*.

## Build System

* [Clean up Makefile by using go
  install](https://github.com/lightningnetwork/lnd/pull/6035).

* [Make etcd max message size
  configurable](https://github.com/lightningnetwork/lnd/pull/6049).

* [Export bitcoind port and other values for itests, useful for
  using itest harness outside of
  lnd](https://github.com/lightningnetwork/lnd/pull/6050).

## Bug fixes

* [Add json flag to
  trackpayment](https://github.com/lightningnetwork/lnd/pull/6060)
* [Clarify invalid config timeout
  constraints](https://github.com/lightningnetwork/lnd/pull/6073)

* [Fix memory corruption in Mission Control
  Store](https://github.com/lightningnetwork/lnd/pull/6068)
 
* [Ensure that the min relay fee is always clamped by our fee
  floor](https://github.com/lightningnetwork/lnd/pull/6076)

* [Clarify log message about not running within
  systemd](https://github.com/lightningnetwork/lnd/pull/6096)

* [Fix Postgres tests](https://github.com/lightningnetwork/lnd/pull/6104)

## RPC Server

* [ChanStatusFlags is now
  exposed](https://github.com/lightningnetwork/lnd/pull/5971) inside
  WaitingCloseResp from calling `PendingChannels`.

* [Fix missing label on streamed
  transactions](https://github.com/lightningnetwork/lnd/pull/5854).

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Bjarne Magnussen
* Elle Mouton
* Harsha Goli
* Martin Habovštiak
* Naveen Srinivasan
* Oliver Gugger
* Yong Yu
