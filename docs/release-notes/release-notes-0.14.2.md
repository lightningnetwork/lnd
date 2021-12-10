# Release Notes

## Bug Fixes

* [Return the nearest known fee rate when a given conf target cannot be found
  from Web API fee estimator.](https://github.com/lightningnetwork/lnd/pull/6062)

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

* [Fix memory corruption in Mission Control
  Store](https://github.com/lightningnetwork/lnd/pull/6068)

## RPC Server

* [ChanStatusFlags is now
  exposed](https://github.com/lightningnetwork/lnd/pull/5971) inside
  WaitingCloseResp from calling `PendingChannels`.

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Harsha Goli
* Martin Habovštiak
* Naveen Srinivasan
* Oliver Gugger
* Yong Yu
