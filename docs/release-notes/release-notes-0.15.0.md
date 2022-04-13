# Release Notes

## Security

* [Misconfigured ZMQ
  setup now gets reported](https://github.com/lightningnetwork/lnd/pull/5710).

## Taproot

The internal on-chain wallet of `lnd` is now able to create and spend from
[Taproot (SegWit v1)
addresses](https://github.com/lightningnetwork/lnd/pull/6263). Using
`lncli newaddress p2tr` will create a new BIP-0086 keyspend only address and
then watch it on chain. Taproot script spends are also supported through the
`signrpc.SignOutputRaw` RPC (`/v2/signer/signraw` in REST).

## `lncli`

* Add [auto-generated command-line completions](https://github.com/lightningnetwork/lnd/pull/4177) 
  for Fish shell.  

* Add [chan_point flag](https://github.com/lightningnetwork/lnd/pull/6152)
  to closechannel command.

* Add [private status](https://github.com/lightningnetwork/lnd/pull/6167)
  to pendingchannels response.

* [Update description for `state` command](https://github.com/lightningnetwork/lnd/pull/6237).

* Add [update node announcement](https://github.com/lightningnetwork/lnd/pull/5587)
  for updating and propagating node information.

## Bug Fixes

* [Pipelining an UpdateFulfillHTLC message now only happens when the related UpdateAddHTLC is locked-in.](https://github.com/lightningnetwork/lnd/pull/6246)

* [Fixed an inactive invoice subscription not removed from invoice
  registry](https://github.com/lightningnetwork/lnd/pull/6053). When an invoice
  subscription is created and canceled immediately, it could be left uncleaned
  due to the cancel signal is processed before the creation. It is now properly
  handled by moving creation before deletion.   

* When the block height+delta specified by a network message is greater than
  the gossiper's best height, it will be considered as premature and ignored.
  [These premature messages are now saved into a cache and processed once the
  height has reached.](https://github.com/lightningnetwork/lnd/pull/6054)

* [Fixed failure to limit our number of hop hints in private invoices](https://github.com/lightningnetwork/lnd/pull/6236).
  When a private invoice is created, and the node had > 20 (our hop hint limit)
  private channels with inbound > invoice amount, hop hint selection would add
  too many hop hints. When a node had many channels meeting this criteria, it 
  could result in an "invoice too large" error when creating invoices. Hints 
  are now properly limited to our maximum of 20.

* [Fixed an edge case where the lnd might be stuck at starting due to channel
  arbitrator relying on htlcswitch to be started
  first](https://github.com/lightningnetwork/lnd/pull/6214).

* [Added signature length
  validation](https://github.com/lightningnetwork/lnd/pull/6314) when calling
  `NewSigFromRawSignature`.

* [Fixed deadlock in invoice
  registry](https://github.com/lightningnetwork/lnd/pull/6332).

* [Fixed an issue that would cause wallet UTXO state to be incorrect if a 3rd
  party sweeps our anchor
  output](https://github.com/lightningnetwork/lnd/pull/6274).

* [Fixed node shutdown in forward interceptor itests](https://github.com/lightningnetwork/lnd/pull/6362).

* [Fixed a bug that would cause lnd to be unable to parse certain PSBT blobs](https://github.com/lightningnetwork/lnd/pull/6383).
 
* [Use normal TCP resolution, instead of Tor DNS resolution, for addresses
   using the all-interfaces IP](https://github.com/lightningnetwork/lnd/pull/6376).

* [Fixed a bug in the `btcwallet` that caused an error to be shown for
  `lncli walletbalance` in existing wallets after upgrading to
  Taproot](https://github.com/lightningnetwork/lnd/pull/6379).

* [Fixed a data race in the websocket proxy
  code](https://github.com/lightningnetwork/lnd/pull/6380).

* [Fixed race condition resulting in MPP payments sometimes getting stuck
  in-flight](https://github.com/lightningnetwork/lnd/pull/6352).

* [Fixed a panic in the Taproot signing part of the `SignOutputRaw` RPC that
  occurred when not all UTXO information was
  specified](https://github.com/lightningnetwork/lnd/pull/6407).

* [Fixed P2TR addresses not correctly being detected as
  used](https://github.com/lightningnetwork/lnd/pull/6389).

## Routing

* [Add a new `time_pref` parameter to the QueryRoutes and SendPayment APIs](https://github.com/lightningnetwork/lnd/pull/6024) that
  allows the caller to control the trade-off between payment speed and cost in
  pathfinding.

## Misc

* [An example systemd service file](https://github.com/lightningnetwork/lnd/pull/6033)
  for running lnd alongside a bitcoind service is now provided in
  `contrib/init/lnd.service`.

* [Allow disabling migrations if the database backend passed to `channeldb` was
  opened in read-only mode](https://github.com/lightningnetwork/lnd/pull/6084).

* [Disable compiler optimizations](https://github.com/lightningnetwork/lnd/pull/6105)
  when building `lnd-debug` and `lncli-debug`. It helps when stepping through the code
  with a debugger like Delve.
  
* A new command `lncli leaseoutput` was [added](https://github.com/lightningnetwork/lnd/pull/5964).

* [Consolidated many smaller docs/typo/trivial fixes from PRs that were stuck
  in review because of unmet contribution guideline
  requirements](https://github.com/lightningnetwork/lnd/pull/6080).

* [A nightly build of the `lnd` docker image is now created
  automatically](https://github.com/lightningnetwork/lnd/pull/6160).
  
* Add default values to [walletrpc.ListUnspent RPC call](https://github.com/lightningnetwork/lnd/pull/6190).

* [Add `.vs/` folder to `.gitignore`](https://github.com/lightningnetwork/lnd/pull/6178). 

* [Chain backend healthchecks disabled for --nochainbackend mode](https://github.com/lightningnetwork/lnd/pull/6184)

* [The `tlv` package was refactored into its own Golang
  submodule](https://github.com/lightningnetwork/lnd/pull/6283).

* The `tor` package was refactored into its own Golang submodule and a new
  process for changing and tagging submodules was introduced in a series of
  3 PRs ([#6350](https://github.com/lightningnetwork/lnd/pull/6350),
  [#6355](https://github.com/lightningnetwork/lnd/pull/6350) and
  [#6356](https://github.com/lightningnetwork/lnd/pull/6356)).

* [Source repository can now be specified for Docker image builds](https://github.com/lightningnetwork/lnd/pull/6300)

* [The new `btcsuite/btcd/btcec/v2` and the moved `btcsuite/btcd/btcutil`
  modules were integrated into `lnd` as a preparation for basic Taproot
  support](https://github.com/lightningnetwork/lnd/pull/6285).

* [Make etcd leader election session
  TTL](https://github.com/lightningnetwork/lnd/pull/6342) configurable.

* [Fix race condition in the htlc interceptor unit
  test](https://github.com/lightningnetwork/lnd/pull/6353).

* [A new config option, `pending-commit-interval` is
  added](https://github.com/lightningnetwork/lnd/pull/6186). This value
  specifies the maximum duration it allows for a remote peer to respond to a
  locally initiated commitment update.

* [`macos` and `apple` Makefile tasks have been added.](https://github.com/lightningnetwork/lnd/pull/6373)

  The `macos` task uses `gomobile` to build an `XCFramework` that can be used to
  embed lnd to macOS apps, similar to how the `ios` task builds for iOS.

  The `apple` task uses `gomobile` to build an `XCFramework` that can be used to
  embed lnd to both iOS and macOS apps.


## RPC Server

* [Add value to the field
  `remote_balance`](https://github.com/lightningnetwork/lnd/pull/5931) in
  `pending_force_closing_channels` under `pendingchannels` whereas before was
  empty(zero).
* The graph's [diameter is calculated](https://github.com/lightningnetwork/lnd/pull/6066)
  and added to the `getnetworkinfo` output.

* [Add dev only RPC subserver and the devrpc.ImportGraph
  call](https://github.com/lightningnetwork/lnd/pull/6149)
  
* [Extend](https://github.com/lightningnetwork/lnd/pull/6177) the HTLC
  interceptor API to provide more control over failure messages. With this
  change, it allows encrypted failure messages to be returned to the sender.
  Additionally it is possible to signal a malformed htlc.

* Add an [always on](https://github.com/lightningnetwork/lnd/pull/6232) mode to
  the HTLC interceptor API. This enables interception applications where every
  packet must be intercepted.

* Add [destination output information](https://github.com/lightningnetwork/lnd/pull/5476)
  to the transaction structure returned from the RPC `GetTransactions` and when
  subscribed with `SubscribeTransactions`.

* [Support for making routes with the legacy onion payload format via `SendToRoute` has been removed.](https://github.com/lightningnetwork/lnd/pull/6385)

* Close a gap in the HTLC interceptor API by [intercepting htlcs in the on-chain
  resolution flow](https://github.com/lightningnetwork/lnd/pull/6219) too.

## Database

* [Add ForAll implementation for etcd to speed up
  graph cache at startup](https://github.com/lightningnetwork/lnd/pull/6136)

* [Improve validation of a PSBT packet when handling a request to finalize it.](https://github.com/lightningnetwork/lnd/pull/6217)

* [Add new Peers subserver](https://github.com/lightningnetwork/lnd/pull/5587) with a new endpoint for updating the `NodeAnnouncement` data without having to restart the node.

* Add [htlc expiry protection](https://github.com/lightningnetwork/lnd/pull/6212)
to the htlc interceptor API.

## Documentation

* Improved instructions on [how to build lnd for mobile](https://github.com/lightningnetwork/lnd/pull/6085).
* [Log force-close related messages on "info" level](https://github.com/lightningnetwork/lnd/pull/6124).

## Monitoring

A new [flag (`--prometheus.perfhistograms`) has been added to enable export of
gRPC performance metrics (latency to process `GetInfo`, etc)](https://github.com/lightningnetwork/lnd/pull/6224).

## Code Health

### Code cleanup, refactor, typo fixes

* [Refactored itest to better manage contexts inside integration tests](https://github.com/lightningnetwork/lnd/pull/5756).

* [Fix itest not picking up local config file or creating directories in home
  dir of the user](https://github.com/lightningnetwork/lnd/pull/6202).

* [A refactor of `SelectHopHints`](https://github.com/lightningnetwork/lnd/pull/6182) 
  allows code external to lnd to call the function, where previously it would 
  require access to lnd's internals.

* [rpc-check fails if it finds any changes](https://github.com/lightningnetwork/lnd/pull/6207/)
  including new and deleted files.

* [The `golangci-lint` package was updated and new linters were
  enabled](https://github.com/lightningnetwork/lnd/pull/6244).

* The linting process now runs [inside a docker
  container](https://github.com/lightningnetwork/lnd/pull/6248) to fix
  versioning issues between projects.

* The [`whitespace` linter](https://github.com/lightningnetwork/lnd/pull/6270)
  was enabled to make sure multi-line `if` conditions and function/method
  declarations are followed by an empty line to improve readability.
  **Note to developers**: please make sure you delete the old version of
  `golangci-lint` in your `$GOPATH/bin` directory. `make lint` does not
  automatically replace it with the new version if the binary already exists!
  
* [`ChannelLink` in the `htlcswitch` now performs a 2-way handoff instead of a 1-way handoff with its `ChannelArbitrator`.](https://github.com/lightningnetwork/lnd/pull/6221)

* [The channel-commit-interval is now clamped to a reasonable timeframe of 1h.](https://github.com/lightningnetwork/lnd/pull/6220)

* [A function in the gossiper `processNetworkAnnouncements` has been refactored for readability and for future deduplication efforts.](https://github.com/lightningnetwork/lnd/pull/6278)

# Contributors (Alphabetical Order)

* 3nprob
* Andras Banki-Horvath
* Andreas Schjønhaug
* asvdf
* bitromortac
* Bjarne Magnussen
* BTCparadigm
* Carla Kirk-Cohen
* Carsten Otto
* Dan Bolser
* Daniel McNally
* Elle Mouton
* ErikEk
* Eugene Siegel
* Hampus Sjöberg
* henta
* Joost Jager
* Jordi Montes
* LightningHelper
* Liviu
* mateuszmp
* Naveen Srinivasan
* Olaoluwa Osuntokun
* randymcmillan
* Rong Ou
* Thebora Kompanioni
* Torkel Rogstad
* Vsevolod Kaganovych
* Yong Yu
