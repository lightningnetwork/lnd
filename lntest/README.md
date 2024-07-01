# `lntest`

`lntest` is a package which holds the components used for the `lnd`’s
integration tests. It is responsible for managing `lnd` nodes, chain backends
and miners, advancing nodes’ states and providing assertions.

### Quick Start

A simple example to run the integration test.

```go
func TestFoo(t *testing.T) {
	// Get the binary path and setup the harness test.
	//
	// TODO: define the binary path to lnd and the name of the database
	// backend.
	harnessTest := lntemp.SetupHarness(t, binary, *dbBackendFlag)
	defer harnessTest.Stop()

	// Setup standby nodes, Alice and Bob, which will be alive and shared
	// among all the test cases.
	harnessTest.SetupStandbyNodes()

	// Run the subset of the test cases selected in this tranche.
	//
	// TODO: define your own testCases.
	for _, tc := range testCases {
		tc := tc

		t.Run(tc.Name, func(st *testing.T) {
			// Create a separate harness test for the testcase to
			// avoid overwriting the external harness test that is
			// tied to the parent test.
			ht := harnessTest.Subtest(st)

			// Run the test cases.
			ht.RunTestCase(tc)
		})
	}
}
```

### Package Structure

This package has four major components, `HarnessTest`, `HarnessMiner`,
`node.HarnessNode` and `rpc.HarnessRPC`, with the following architecture,

```
+----------------------------------------------------------+
|                                                          |
|                        HarnessTest                       |
|                                                          |
| +----------------+  +----------------+  +--------------+ |
| |   HarnessNode  |  |   HarnessNode  |  | HarnessMiner | |
| |                |  |                |  +--------------+ |
| | +------------+ |  | +------------+ |                   |
| | | HarnessRPC | |  | | HarnessRPC | |  +--------------+ |
| | +------------+ |  | +------------+ |  | HarnessMiner | |
| +----------------+  +----------------+  +--------------+ |
+----------------------------------------------------------+
```

- `HarnessRPC` holds all the RPC clients and adds a layer over all the RPC
  methods to assert no error happened at the RPC level.

- `HarnessNode` builds on top of the `HarnessRPC`. It is responsible for
  managing the `lnd` node, including start and stop pf the `lnd` process,
  authentication of the gRPC connection, topology subscription(`NodeWatcher`)
  and maintains an internal state(`NodeState`).

- `HarnessMiner` builds on top of `btcd`’s `rcptest.Harness` and is responsible
  for managing blocks and the mempool.

- `HarnessTest` builds on top of `testing.T` and can be viewed as the assertion
  machine. It provides multiple ways to initialize a node, such as with/without
  seed, backups, etc. It also handles interactions between nodes like
  connecting nodes and opening/closing channels so it’s easier to acquire or
  validate a desired test states such as node’s balance, mempool condition,
  etc.

### Standby Nodes

Standby nodes are `HarnessNode`s created when initializing the integration test
and stay alive across all the test cases. Creating a new node is not without a
cost. With block height increasing, it takes significantly longer to initialize
a new node and wait for it to be synced. Standby nodes, however, don’t have
this problem as they are digesting blocks all the time. Thus, it’s encouraged to
use standby nodes wherever possible.

Currently, there are two standby nodes, Alice and Bob. Their internal states are
recorded and taken into account when `HarnessTest` makes assertions. When
making a new test case using `Subtest`, there’s a cleanup function which
further validates the current test case has no dangling uncleaned states, such
as transactions left in mempool, open channels, etc.

### Different Code Used in `lntest`

Since the miner in `lntest` uses regtest, it has a very fast block production
rate, which is the greatest difference between the conditions it simulates and
the real-world has. Aside from that, `lnd` has several places that use
different code, which is triggered by the build flag `integration`, to speed up
the tests. They are summarized as followings,

1. `funding.checkPeerChannelReadyInterval`, which is used when we wait for the
   peer to send us `ChannelReady`. This value is 1 second in `lnd`, and 10
   milliseconds in `lntest`.
2. `lncfg.ProtocolOptions`, which is used to specify protocol flags. In `lnd`,
   anchor and script enforced lease are enabled by default, while in `lntest`,
   they are disabled by default.
3. Reduced scrypt parameters are used in `lntest`. In `lnd`, the parameters N,
   R, and P are imported from `snacl`, while in `lntest` they are replaced with
   `waddrmgr.FastScryptOptions`. Both `macaroon` and `aezeed` are affected.
4. The method, `nextRevocationProducer`, defined in `LightningWallet` is
   slightly different. For `lnwallet`, it will check a special pre-defined
   channel ID to test restoring channel backups created with the old revocation
   root derivation method.
