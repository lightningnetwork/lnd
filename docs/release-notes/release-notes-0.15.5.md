# Release Notes

## Bug Fixes

* [A Taproot related key tweak issue was fixed in `btcd` that affected remote
  signing setups](https://github.com/lightningnetwork/lnd/pull/7130).

* [Taproot changes addresses are now used by default for the `SendCoins`
  RPC](https://github.com/lightningnetwork/lnd/pull/7193).

* [A 1 second interval has been added between `FundingLocked` receipt
  checks](https://github.com/lightningnetwork/lnd/pull/7095). This reduces idle
  CPU usage for pending/dangling funding attempts.

# Contributors (Alphabetical Order)

* Olaoluwa Osuntokun
* Oliver Gugger
* Yong Yu
