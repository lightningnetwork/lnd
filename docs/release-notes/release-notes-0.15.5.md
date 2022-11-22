# Release Notes

## Bug Fixes

* [A Taproot related key tweak issue was fixed in `btcd` that affected remote
  signing setups](https://github.com/lightningnetwork/lnd/pull/7130).

* [Taproot changes addresses are now used by default for the `SendCoins`
  RPC](https://github.com/lightningnetwork/lnd/pull/7193).

* [A 1 second interval has been added between `FundingLocked` receipt
  checks](https://github.com/lightningnetwork/lnd/pull/7095). This reduces idle
  CPU usage for pending/dangling funding attempts.

* [The wallet birthday is now used properly when creating a watch-only wallet
  to avoid scanning the whole
  chain](https://github.com/lightningnetwork/lnd/pull/7056).

* Taproot outputs are now correctly shown as `SCRIPT_TYPE_WITNESS_V1_TAPROOT` in
  the output of `GetTransactions` (`lncli listchaintxns`).

# Contributors (Alphabetical Order)

* Olaoluwa Osuntokun
* Oliver Gugger
* Yong Yu
