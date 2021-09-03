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

* LND now [reports to systemd](https://github.com/lightningnetwork/lnd/pull/5536)
  that RPC is ready (port bound, certificate generated, macaroons created,
  in case of `wallet-unlock-password-file` wallet unlocked). This can be used to
  avoid misleading error messages from dependent services if they use `After`
  systemd option.

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

## Security 

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

## Documentation

* [Outdated warning about unsupported pruning was replaced with clarification that LND **does**
  support pruning](https://github.com/lightningnetwork/lnd/pull/5553)

* [Clarified 'ErrReservedValueInvalidated' error string](https://github.com/lightningnetwork/lnd/pull/5577)
   to explain that the error is triggered by a transaction that would deplete
   funds already reserved for potential future anchor channel closings (via
   CPFP) and that more information (e.g., specific sat amounts) can be found
   in the debug logs.

## Misc

* The direct use of certain syscalls in packages such as `bbolt` or `lnd`'s own
  `healthcheck` package made it impossible to import `lnd` code as a library
  into projects that are compiled to WASM binaries. [That problem was fixed by
  guarding those syscalls with build tags](https://github.com/lightningnetwork/lnd/pull/5526).

## Code Health

### Code cleanup, refactor, typo fixes

* [Refactor the interaction between the `htlcswitch` and `peer` packages for cleaner separation.](https://github.com/lightningnetwork/lnd/pull/5603)

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

* [Flake fix in async bidirectional payment test](https://github.com/lightningnetwork/lnd/pull/5607).

* [Fixed context timeout when closing channels in tests](https://github.com/lightningnetwork/lnd/pull/5616).

* [Fixed transaction not found in mempool flake in commitment deadline itest](https://github.com/lightningnetwork/lnd/pull/5615).

* [Fixed a missing import and git tag in the healthcheck package](https://github.com/lightningnetwork/lnd/pull/5582).

* [Fixed a data race in payment unit test](https://github.com/lightningnetwork/lnd/pull/5573).

* [Missing dots in cmd interface](https://github.com/lightningnetwork/lnd/pull/5535).

* [Link channel point logging](https://github.com/lightningnetwork/lnd/pull/5508)

* [Canceling the chain notifier no longer logs certain errors](https://github.com/lightningnetwork/lnd/pull/5676)

* [Fixed context leak in integration tests, and properly handled context
  timeout](https://github.com/lightningnetwork/lnd/pull/5646).

* [Removed nested db tx](https://github.com/lightningnetwork/lnd/pull/5643)

## Database

* [Ensure single writer for legacy
  code](https://github.com/lightningnetwork/lnd/pull/5547) when using etcd
  backend.

* [Optimized payment sequence generation](https://github.com/lightningnetwork/lnd/pull/5514/)
  to make LNDs payment throughput (and latency) with better when using etcd.

* [More robust commit queue design](https://github.com/lightningnetwork/lnd/pull/5513)
  to make it less likely that we retry etcd transactions and make the commit
  queue more scalable.

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

A bug has been fixed that would cause `lnd` to [try to bootstrap using the
currnet DNS seeds when in SigNet
mode](https://github.com/lightningnetwork/lnd/pull/5564).

[A validation check for sane `CltvLimit` and `FinalCltvDelta` has been added for `REST`-initiated payments.](https://github.com/lightningnetwork/lnd/pull/5591)

[A bug has been fixed with Neutrino's `RegisterConfirmationsNtfn` and `RegisterSpendNtfn` calls that would cause notifications to be missed.](https://github.com/lightningnetwork/lnd/pull/5453)

## Documentation 

The [code contribution guidelines have been updated to mention the new
requirements surrounding updating the release notes for each new
change](https://github.com/lightningnetwork/lnd/pull/5613). 

# Contributors (Alphabetical Order)
* Andras Banki-Horvath
* de6df1re
* ErikEk
* Eugene Siegel
* Martin Habovstiak
* Oliver Gugger
* Wilmer Paulino
* xanoni
* Yong Yu
* Zero-1729
