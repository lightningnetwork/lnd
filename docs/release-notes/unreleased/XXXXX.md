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

* `channeldb`: counting the public anchor channels the wallet keeps a reserve
  for (computed inside `WalletBalance` for the reserved-anchor balance, and
  when checking the reserved value during channel opens) now reads only each
  channel's info record instead of fully deserializing its commitments and
  revocation state. On remote SQL (PostgreSQL) backends this cuts the
  per-channel reads the count performs, for both the open and the
  pending-close channels it visits.
  ([#XXXXX](https://github.com/lightningnetwork/lnd/pull/XXXXX))
