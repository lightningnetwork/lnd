# Release Notes

## Networking & Tor

A new flag has been added to enable a hybrid tor connectivity mode, where tor
is only used for onion address connections, and clearnet for everything else.
This new behavior can be added using the `tor.skip-proxy-for-clearnet-targets`
flag.

## LN Peer-to-Peer Netowrk

### Bitcoin Blockheaders in Ping Messages

[In this release, we implement a long discussed mechanism to use the Lightning
Network as a redundant block header
source](https://github.com/lightningnetwork/lnd/pull/5621). By sending our
latest block header with each ping message, we give peers another source
(outside of the Bitcoin P2P network) they can use to spot check their chain
state. Peers can also use this information to detect if they've been eclipsed
from the traditional Bitcoin P2P network itself.

As is, we only send this data in Ping messages (which are periodically sent),
in the future we could also move to send them as the partial payload for our
pong messages, and also randomize the payload size requested as well.

The `ListPeers` RPC call will now also include a hex encoded version of the
last ping message the peer has sent to us.

## Backend Enhancements & Optimizations

### Full remote database support

`lnd` now stores [all its data in the same remote/external
database](https://github.com/lightningnetwork/lnd/pull/5484) such as `etcd`
instead of only the channel state and wallet data. This makes `lnd` fully
stateless and therefore makes switching over to a new leader instance almost
instantaneous. Read the [guide on leader
election](https://github.com/lightningnetwork/lnd/blob/master/docs/leader_election.md)
for more information.

### Postgres database support

This release adds [support for Postgres as a database
backend](https://github.com/lightningnetwork/lnd/pull/5366) to lnd. Postgres
has several advantages over the default bbolt backend:
* Better handling of large data sets.
* On-the-fly database compaction (auto vacuum).
* Database replication.
* Inspect data while lnd is running (bbolt opens the database exclusively).
* Usage of industry-standard tools to manage the stored data, get performance
  metrics, etc.

Furthermore, the SQL platform opens up possibilities to improve lnd's
performance in the future. Bbolt's single-writer model is a severe performance
bottleneck, whereas Postgres offers a variety of locking models. Additionally,
structured tables reduce the need for custom serialization/deserialization code
in `lnd`, saving developer time and limiting the potential for bugs.

Instructions for enabling Postgres can be found in
[docs/postgres.md](../postgres.md).

### In-memory path finding

Finding a path through the channel graph for sending a payment doesn't involve
any database queries anymore. The [channel graph is now kept fully
in-memory](https://github.com/lightningnetwork/lnd/pull/5642) for up a massive
performance boost when calling `QueryRoutes` or any of the `SendPayment`
variants. Keeping the full graph in memory naturally comes with increased RAM
usage. Users running `lnd` on low-memory systems are advised to run with the
`routing.strictgraphpruning=true` configuration option that more aggressively
removes zombie channels from the graph, reducing the number of channels that
need to be kept in memory.

## Protocol Extensions

### Explicit Channel Negotiation

[A new protocol extension has been added known as explicit channel negotiation]
(https://github.com/lightningnetwork/lnd/pull/5669). This allows a channel
initiator to signal their desired channel type to use with the remote peer. If
the remote peer supports said channel type and agrees, the previous implicit
negotiation based on the shared set of feature bits is bypassed, and the
proposed channel type is used.

## RPC Server

* [Return payment address and add index from
  addholdinvoice call](https://github.com/lightningnetwork/lnd/pull/5533).

* [The versions of several gRPC related libraries were bumped and the main
  `rpc.proto` was renamed to
  `lightning.proto`](https://github.com/lightningnetwork/lnd/pull/5473) to fix
  a warning related to protobuf file name collisions.

* [Stub code for interacting with `lnrpc` from a WASM context through JSON 
  messages was added](https://github.com/lightningnetwork/lnd/pull/5601).

* The updatechanpolicy call now [detects invalid and pending channels, and 
  returns a policy update failure report](https://github.com/lightningnetwork/lnd/pull/5405).

* LND now [reports to systemd](https://github.com/lightningnetwork/lnd/pull/5536)
  that RPC is ready (port bound, certificate generated, macaroons created,
  in case of `wallet-unlock-password-file` wallet unlocked). This can be used to
  avoid misleading error messages from dependent services if they use `After`
  systemd option.

* [Delete a specific payment, or its failed HTLCs](https://github.com/lightningnetwork/lnd/pull/5660).

* A new state, [`WalletState_SERVER_ACTIVE`](https://github.com/lightningnetwork/lnd/pull/5637),
  is added to the state server. This state indicates whether the `lnd` server
  and all its subservers have been fully started or not.

* [Adds an option to the BakeMacaroon rpc "allow-external-permissions,"](https://github.com/lightningnetwork/lnd/pull/5304) which makes it possible to bake a macaroon with external permissions. That way, the baked macaroons can be used for services beyond LND. Also adds a new CheckMacaroonPermissions rpc that checks that the macaroon permissions and other restrictions are being followed. It can also check permissions not native to LND.

* [A new RPC middleware
  interceptor](https://github.com/lightningnetwork/lnd/pull/5101) was added that
  allows external tools to hook into `lnd`'s RPC server and intercept any
  requests made with custom macaroons (and also the responses to those
  requests).
  
* [Adds NOT_FOUND status code for LookupInvoice](https://github.com/lightningnetwork/lnd/pull/5768)

### Batched channel funding

[Multiple channels can now be opened in a single
transaction](https://github.com/lightningnetwork/lnd/pull/5356) in a safer and
more straightforward way by using the `BatchOpenChannel` RPC or the command line
version of that RPC called `lncli batchopenchannel`. More information can be
found in the [PSBT
documentation](../psbt.md#use-the-batchopenchannel-rpc-for-safe-batch-channel-funding).

## Wallet

* It is now possible to fund a psbt [without specifying any
  outputs](https://github.com/lightningnetwork/lnd/pull/5442). This option is
  useful for CPFP bumping of unconfirmed outputs or general utxo consolidation.
* The internal wallet can now also be created or restored by using an [extended
  master root key (`xprv`) instead of an
  `aezeed`](https://github.com/lightningnetwork/lnd/pull/4717) only. This allows
  wallet integrators to use existing seed mechanism that might already be in
  place. **It is still not supported to use the same seed/root key on multiple
  `lnd` instances simultaneously** though.

* [Publish transaction is now reachable through 
  lncli](https://github.com/lightningnetwork/lnd/pull/5460).

* Prior to this release, when running on `simnet` or `regtest`, `lnd` would
  skip the check on wallet synchronization during its startup. In doing so, the
  integration test can bypass the rule set by `bitcoind`, which considers the
  node is out of sync when the last block is older than 2 hours([more
  discussion](https://github.com/lightningnetwork/lnd/pull/4685#discussion_r503080709)).
  This synchronization check is put back now as we want to make the integration
  test more robust in catching real world situations. This also means it might
  take longer to start a `lnd` node when running in `simnet` or `regtest`,
  something developers need to watch out from this release.

## Security 

* The release signature verification script [was overhauled to fix some possible
  attack vectors and user
  errors](https://github.com/lightningnetwork/lnd/pull/5053). The public keys
  used to verify the signatures against are no longer downloaded form Keybase
  but instead are kept in the `lnd` git repository. This allows for a more
  transparent way of keeping track of changes to the signing keys.

### Admin macaroon permissions

The default file permissions of admin.macaroon were [changed from 0600 to
0640](https://github.com/lightningnetwork/lnd/pull/5534). This makes it easier
to allow other users to manage LND. This is safe on common Unix systems
because they always create a new group for each user.

If you use a strange system or changed group membership of the group running LND
you may want to check your system to see if it introduces additional risk for
you.

## Safety

* Locally force closed channels are now [kept in the channel.backup file until
  their time lock has fully matured](https://github.com/lightningnetwork/lnd/pull/5528).

* [Cooperative closes optimistically shutdown the associated `link` before closing the channel.](https://github.com/lightningnetwork/lnd/pull/5618)

## Build System

* [A new pre-submit check has been
  added](https://github.com/lightningnetwork/lnd/pull/5520) to ensure that all
  PRs ([aside from merge
  commits](https://github.com/lightningnetwork/lnd/pull/5543)) add an entry in
  the release notes folder that at least links to PR being added.

* [A new build target itest-race](https://github.com/lightningnetwork/lnd/pull/5542) 
  to help uncover undetected data races with our itests.

* [The itest error whitelist check was removed to reduce the number of failed
  Travis builds](https://github.com/lightningnetwork/lnd/pull/5588).

* [A flake in the Neutrino integration tests with anchor sweeps was 
  addressed](https://github.com/lightningnetwork/lnd/pull/5509).

* [The `lnwire` fuzz tests have been fixed and now run without crashing.](https://github.com/lightningnetwork/lnd/pull/5395)

* [A flake in the race unit
  tests](https://github.com/lightningnetwork/lnd/pull/5659) was addressed that
  lead to failed tests sometimes when the CPU of the GitHub CI runner was
  strained too much.

* [Reduce the number of parallel itest runs to 2 on
  ARM](https://github.com/lightningnetwork/lnd/pull/5731).

* [Fix Travis itest parallelism](https://github.com/lightningnetwork/lnd/pull/5734)

## Documentation

* [Outdated warning about unsupported pruning was replaced with clarification that LND **does**
  support pruning](https://github.com/lightningnetwork/lnd/pull/5553).

* [Clarified 'ErrReservedValueInvalidated' error string](https://github.com/lightningnetwork/lnd/pull/5577)
   to explain that the error is triggered by a transaction that would deplete
   funds already reserved for potential future anchor channel closings (via
   CPFP) and that more information (e.g., specific sat amounts) can be found
   in the debug logs.

* [Updated C# grpc docs to use Grpc.Net.Client](https://github.com/lightningnetwork/lnd/pull/5766).
  The Grpc.Core NuGet package is in maintenance mode. Grpc.Net.Client is now the 
  [recommended](https://github.com/grpc/grpc-dotnet#grpc-for-net-is-now-the-recommended-implementation)
  implementation.

## Misc

* The direct use of certain syscalls in packages such as `bbolt` or `lnd`'s own
  `healthcheck` package made it impossible to import `lnd` code as a library
  into projects that are compiled to WASM binaries. [That problem was fixed by
  guarding those syscalls with build tags](https://github.com/lightningnetwork/lnd/pull/5526).

* The only way to retrieve hophints for a given node was to create an invoice
  with the `addInvoice` rpc interface. However, now the function has been
  [exposed in the go package `invoicesrpc`](https://github.com/lightningnetwork/lnd/pull/5697).

* The `DeleteAllPayments` and `DeletePayment` RPC methods can now be called from
  the command line with the [new 
  `lncli deletepayments`](https://github.com/lightningnetwork/lnd/pull/5699)
  command.

## Code Health

### Code cleanup, refactor, typo fixes

* [Refactor the interaction between the `htlcswitch` and `peer` packages for cleaner separation.](https://github.com/lightningnetwork/lnd/pull/5603)

* [Moved the original breach handling and timelock UTXO handling into the contract court package](https://github.com/lightningnetwork/lnd/pull/5745)

* [Unused error check 
  removed](https://github.com/lightningnetwork/lnd/pull/5537).

* [Shorten Pull Request check list by referring to the CI checks that are 
  in place](https://github.com/lightningnetwork/lnd/pull/5545).

* [Added minor fixes to contribution guidelines](https://github.com/lightningnetwork/lnd/pull/5503).

* [Fixed typo in `dest_custom_records` description comment](https://github.com/lightningnetwork/lnd/pull/5541).

* [Fixed payment test error message.](https://github.com/lightningnetwork/lnd/pull/5559)

* [Bumped version of `github.com/miekg/dns` library to fix a Dependabot
  alert](https://github.com/lightningnetwork/lnd/pull/5576).

* [Fixed timeout flakes in async payment benchmark tests](https://github.com/lightningnetwork/lnd/pull/5579).

* [State, subscribechannelevents, subscribepeerevents, subscribeinvoices, subscribetransactions, 
  subscribechannelgraph and subscribechannelbackups no longer logs certain errors](https://github.com/lightningnetwork/lnd/pull/5695).

* [Flake fix in async bidirectional payment test](https://github.com/lightningnetwork/lnd/pull/5607).

* [Fixed context timeout when closing channels in tests](https://github.com/lightningnetwork/lnd/pull/5616).

* [Fixed transaction not found in mempool flake in commitment deadline itest](https://github.com/lightningnetwork/lnd/pull/5615).

* [Fixed a missing import and git tag in the healthcheck package](https://github.com/lightningnetwork/lnd/pull/5582).

* [Fixed a data race in payment unit test](https://github.com/lightningnetwork/lnd/pull/5573).

* [Missing dots in cmd interface](https://github.com/lightningnetwork/lnd/pull/5535).

* [Link channel point logging](https://github.com/lightningnetwork/lnd/pull/5508).

* [Canceling the chain notifier no longer logs certain errors](https://github.com/lightningnetwork/lnd/pull/5676).

* [Fixed context leak in integration tests, and properly handled context
  timeout](https://github.com/lightningnetwork/lnd/pull/5646).

* [Removed nested db tx](https://github.com/lightningnetwork/lnd/pull/5643).

* [Fixed wallet recovery itests on Travis ARM](https://github.com/lightningnetwork/lnd/pull/5688).

* [Integration tests save embedded etcd logs to help debugging flakes](https://github.com/lightningnetwork/lnd/pull/5702).

* [Replace reference to JWT library with CVE](https://github.com/lightningnetwork/lnd/pull/5737)

* [Fixed restore backup file test flake with bitcoind](https://github.com/lightningnetwork/lnd/pull/5637).

* [Timing fix in AMP itest](https://github.com/lightningnetwork/lnd/pull/5725).

* [Upgraded miekg/dns to improve the security posture](https://github.com/lightningnetwork/lnd/pull/5738).

* [server.go: dedupe pubkey output in debug/log msgs](https://github.com/lightningnetwork/lnd/pull/5722).

* [Fixed flakes caused by graph topology subcription](https://github.com/lightningnetwork/lnd/pull/5611).

* [Order of the start/stop on subsystems are changed to promote better safety](https://github.com/lightningnetwork/lnd/pull/1783).

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.
* When starting/restarting, `lnd` will [clean forwarding packages, payment
  circuits and keystones](https://github.com/lightningnetwork/lnd/pull/4364)
  for closed channels, which will potentially free up disk space for long
  running nodes that have lots of closed channels.

* [Optimized payment sequence generation](https://github.com/lightningnetwork/lnd/pull/5514/)
  to make LNDs payment throughput (and latency) with better when using etcd.

* [More robust commit queue design](https://github.com/lightningnetwork/lnd/pull/5513)
  to make it less likely that we retry etcd transactions and make the commit
  queue more scalable.

* [Flatten the payment-htlcs-bucket](https://github.com/lightningnetwork/lnd/pull/5635)
  in order to make it possible to prefetch all htlc attempts of a payment in one
  DB operation. Migration may fail for extremely large DBs with many payments
  (10+ million). Be careful and backup your `channel.db` if you have that many
  payments. Deleting all failed payments beforehand makes migration safer and
  faster too.

* [Prefetch payments on hot paths](https://github.com/lightningnetwork/lnd/pull/5640)
  to reduce roundtrips to the remote DB backend.

## Performance improvements

* [Update MC store in blocks](https://github.com/lightningnetwork/lnd/pull/5515)
  to make payment throughput better when using etcd.

* [The `lnwire` package now uses a write buffer pool](https://github.com/lightningnetwork/lnd/pull/4884)
  when encoding/decoding messages. Such that most of the heap escapes are fixed,
  resulting in less memory being used when running `lnd`.

* [`lnd` will now no longer (in a steady state) need to open a new database
  transaction each time a private key needs to be derived for signing or ECDH
  operations]https://github.com/lightningnetwork/lnd/pull/5629). This results
  in a massive performance improvement across several routine operations at the

* [When decrypting incoming encrypted brontide messages on the wire, we'll now
  properly re-use the buffer that was allocated for the ciphertext to store the
  plaintext]https://github.com/lightningnetwork/lnd/pull/5622). When combined
  with the buffer pool, this ensures that we no longer need to allocate a new
  buffer each time we decrypt an incoming message, as we
  recycle these buffers in the peer.

## Log system

* [Save compressed log files from logrorate during 
  itest](https://github.com/lightningnetwork/lnd/pull/5354).

## Bug Fixes

* A bug has been fixed that would cause `lnd` to [try to bootstrap using the
  currnet DNS seeds when in SigNet
  mode](https://github.com/lightningnetwork/lnd/pull/5564).

* [A validation check for sane `CltvLimit` and `FinalCltvDelta` has been added
  for `REST`-initiated
  payments](https://github.com/lightningnetwork/lnd/pull/5591).

* [A bug has been fixed with Neutrino's `RegisterConfirmationsNtfn` and
  `RegisterSpendNtfn` calls that would cause notifications to be
  missed](https://github.com/lightningnetwork/lnd/pull/5453).

* [A bug has been fixed when registering for spend notifications in the 
  `txnotifier`. A re-org notification would previously not be dispatched in
  certain scenarios](https://github.com/lightningnetwork/lnd/pull/5465).

* [Catches up on blocks in the
  router](https://github.com/lightningnetwork/lnd/pull/5315) in order to fix an
  "out of order" error that [crops up](https://github.com/lightningnetwork/lnd/pull/5748).

* [Fix healthcheck might be running after the max number of attempts are
  reached](https://github.com/lightningnetwork/lnd/pull/5686).

* [Fix crash with empty AMP or MPP record in
  invoice](https://github.com/lightningnetwork/lnd/pull/5743).

* [Config setting sync-freelist was ignored in certain
  cases](https://github.com/lightningnetwork/lnd/pull/5527).

* The underlying gRPC connection of a WebSocket is now [properly closed when the
  WebSocket end of a connection is
  closed](https://github.com/lightningnetwork/lnd/pull/5683). A bug with the
  write deadline that caused connections to suddenly break was also fixed in the
  same PR.

## Documentation 

The [code contribution guidelines have been updated to mention the new
requirements surrounding updating the release notes for each new
change](https://github.com/lightningnetwork/lnd/pull/5613). 

# Contributors (Alphabetical Order)
* Alyssa Hertig
* Andras Banki-Horvath
* de6df1re
* ErikEk
* Eugene Siegel
* Harsha Goli
* Jesse de Wit
* Martin Habovstiak
* Naveen Srinivasan
* Oliver Gugger
* Priyansh Rastogi
* Wilmer Paulino
* xanoni
* Yong Yu
* Zero-1729
