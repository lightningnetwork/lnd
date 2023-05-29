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


## Consistent Contract Resolution

* If lnd decides to go to chain for an HTLC, it will now _always_ ensure the
  HTLC is fully swept on the outgoing link. Prior logic would avoid sweeping
  due to negative yield, but combined with other inputs, the HTLC will usually
  be positive yield.

# Contributors (Alphabetical Order)

* Elle Mouton
* Olaoluwa Osuntokun
* Yong Yu
