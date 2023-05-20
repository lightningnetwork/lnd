# Release Notes

## Mempool Optimizations

* Optimized [mempool
  management](https://github.com/lightningnetwork/lnd/pull/7681) to lower the
  CPU usage.

## Misc

* [Re-encrypt/regenerate](https://github.com/lightningnetwork/lnd/pull/7705)
  all macaroon DB root keys on `ChangePassword`/`GenerateNewRootKey`
  respectively.

## Channel Link Bug Fix

* If we detect the remote link is inactive, [we'll now tear down the
  connection](https://github.com/lightningnetwork/lnd/pull/7711) in addition to
  stopping the link's statemachine. If we're persistently connected with the
  peer, then this'll force a reconnect, which may restart things and help avoid
  certain force close scenarios.

# Contributors (Alphabetical Order)

* Elle Mouton
* Olaoluwa Osuntokun
* Yong Yu
