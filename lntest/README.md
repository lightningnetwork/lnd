# `lntest`

`lntest` is a package which holds the components used for the `lnd`’s
integration tests. It is responsible for managing `lnd` nodes, chain backends
and miners, advancing nodes’ states and providing assertions. Overall, it has
four components, `HarnessTest`, `HarnessMiner`, `HarnessNet` and `HarnessNode`,
with the following structure,

```
+--------------------------------------------------------+
|                                                        |
|                       HarnessTest                      |
|                                                        |
| +----------------------------------+  +--------------+ |
| |            HarnessNet            |  | HarnessMiner | |
| |                                  |  +--------------+ |
| | +-------------+  +-------------+ |                   |
| | | HarnessNode |  | HarnessNode | |  +--------------+ |
| | +-------------+  +-------------+ |  | HarnessMiner | |
| +----------------------------------+  +--------------+ |
+--------------------------------------------------------+
```

- `HarnessMiner` builds on top of `btcd`’s `rcptest.Harness` and is responsilbe
  for managing blocks and the mempool.

- `HarnessNode` builds on top of the `lnd`’s RPC clients. It is responsible for
  managing the `lnd` node, including start/stop the `lnd` process,
  authentication, topology subscription and maintains an internal
  state(`nodeState`).

- `HarnessNet` builds on top of `HarnessNode`. It provides multiple ways to
  initialize a node, such as with/without seed, backups, etc. It also handles
  interactions between nodes like connecting nodes and opening/closing
  channels.  This component may eventually be built into `HarnessTest`.

- `HarnessTest` builds on top of `testing.T` and can be viewed as the assertion
  machine. It abstracts direct interactions between the test cases and the
  various running nodes such that it’s easier to acquire or validate a desired
  test states such as node’s balance, mempool condition, etc. 


### Standby Nodes

Standby nodes are `HarnessNode`s created when the integration test initialized
and stay alive across all the test cases. Creating a new node is not without a
cost. With block height increasing, it takes significantly longer to initialize
a new node and wait for it to be synced. Standby nodes, however, don’t have
this problem as they are digesting blocks all the time. Thus it’s encouraged to
use standby nodes wherever possible.

Currently there are two standby nodes, Alice and Bob. Their internal states are
recorded and taken into account when `HarnessTest` makes assertions. When
making a new test case using `Subtest`, there’s a cleanup function which
further validates the current test case has no dangling uncleaned states, such
as transactions left in mempool, open channels, etc.

### Tips on making tests

- Use standby nodes wherever possible, it also has the ability to check whether
  the test has cleaned all its states so it won’t mess with other tests.

- If the test involves reading from a subscription such as invoice, HTLCs,
  channel updates, etc, make sure to initialize the subscription as early as
  possible so the subscription client won’t miss out update events.
