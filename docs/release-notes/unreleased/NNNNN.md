# Bug Fixes

* The btcd chain notifier now [determines historical spends by scanning the
  chain's blocks](https://github.com/lightningnetwork/lnd/pull/NNNNN) instead
  of issuing the deprecated `rescan` command, as the bitcoind notifier already
  does. A historical spend dispatch now always completes, so the spend hint of
  an output script that was found unspent is committed and the dispatch is not
  repeated from the original height hint on every restart. Spends are also
  detected on btcd-compatible backends that do not implement `rescan`.
