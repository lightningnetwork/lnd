# Improvements

## Performance Improvements

* `lnwallet/btcwallet`: `WalletBalance` and `WalletKit.ListAccounts` no longer
  run a full wallet UTXO scan for every key scope only to compute per-account
  balances that were then discarded. Account enumeration now reads just the
  stored account metadata, removing database round trips proportional to the
  number of key scopes times the wallet's UTXO count. This speeds up both
  calls on remote SQL (PostgreSQL) wallet backends, where every key/value
  read is a separate client/server round trip.
  ([#XXXXX](https://github.com/lightningnetwork/lnd/pull/XXXXX))

* `rpcserver`: `WalletBalance` now computes every account's balance in a
  single pass over the wallet's unspent outputs, instead of scanning them
  twice per account. On remote SQL (PostgreSQL) wallet backends this removes
  database round trips proportional to the number of accounts.
  ([#XXXXX](https://github.com/lightningnetwork/lnd/pull/XXXXX))
