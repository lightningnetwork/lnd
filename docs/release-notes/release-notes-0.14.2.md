# Release Notes

## Bug Fixes

* [Return the nearest known fee rate when a given conf target cannot be found
  from Web API fee estimator.](https://github.com/lightningnetwork/lnd/pull/6062)

## Build System

* [Clean up Makefile by using go
  install](https://github.com/lightningnetwork/lnd/pull/6035).

* [Make etcd max message size
  configurable]((https://github.com/lightningnetwork/lnd/pull/6049).

* [Fix Postgres context cancellation](https://github.com/lightningnetwork/lnd/pull/6108)

## RPC Server

* [ChanStatusFlags is now
  exposed](https://github.com/lightningnetwork/lnd/pull/5971) inside
  WaitingCloseResp from calling `PendingChannels`.

# Contributors (Alphabetical Order)

* Andras Banki-Horvath
* Naveen Srinivasan
* Oliver Gugger
* Yong Yu
