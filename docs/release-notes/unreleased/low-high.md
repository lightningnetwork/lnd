# New Features

## Functional Enhancements

* Mission control can now [track a fee
  level](https://github.com/lightningnetwork/lnd/pull/XXXXX) per node. This
  feature is experimental and off by default. A temporary channel failure raises
  the fee level of the node that the payment could not reach, and less so the
  fee levels of its neighbors on the route. Pathfinding then gives a higher
  weight to the channels into that node whose fee is below the fee level, so the
  next attempts prefer channels with higher fees. The recommended value of
  `routerrpc.fee-level.ppm` is 50. `routerrpc.fee-level.max` (default 1000)
  caps the fee level of each node and must be at least the fee level PPM. By
  default, only the node that the payment could not reach and the nodes after
  it get a fee level. `routerrpc.fee-level.symmetric` also raises the fee levels
  of the nodes before it, which can be useful for rebalancing, but then any
  routing node on a route can raise the fee levels of the honest nodes before
  it. **Warning:** the feature trades higher route fees for fewer payment
  attempts, and any routing node on a route can raise fee levels with temporary
  channel failures. Fee levels halve after each `routerrpc.fee-level.decay` and
  are kept in memory only. With `routerrpc.fee-level.load`, mission control
  rebuilds them on startup from the stored payment results of the last four
  half-lives. The rebuild applies the fee level config at startup to past
  results, so do not change `ppm`, `max`, `reach`, `decay` or `symmetric`
  before a restart while this option is on. The feature is off by default
  (`routerrpc.fee-level.ppm=0`).

## RPC Additions

* `SetMissionControlConfig` and `GetMissionControlConfig` [include the fee
  level parameters](https://github.com/lightningnetwork/lnd/pull/XXXXX), and
  `QueryMissionControl` returns the fee level of each node. A
  `SetMissionControlConfig` request without fee level parameters keeps the
  current ones.

## lncli Additions

* `lncli setmccfg` has [new fee level
  flags](https://github.com/lightningnetwork/lnd/pull/XXXXX):
  `--feelevelppm`, `--feelevelmax`, `--feelevelreach`, `--feeleveldecay` and
  `--feelevelsymmetric`. Flags that are not set keep their current values.
