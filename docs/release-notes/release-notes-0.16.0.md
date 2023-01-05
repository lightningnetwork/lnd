# Release Notes

## Peer to Peer Behavior

`lnd` will now [properly prioritize sending out gossip updates generated
locally to all connected
peers](https://github.com/lightningnetwork/lnd/pull/7239), regardless of their
current gossip sync query status.


## BOLT Specs

* Warning messages from peers are now recognized and
  [logged](https://github.com/lightningnetwork/lnd/pull/6546) by lnd.

* Decrypt onion failure messages with a [length greater than 256
  bytes](https://github.com/lightningnetwork/lnd/pull/6913). This moves LND
  closer to being spec compliant.

## RPC

* The `RegisterConfirmationsNtfn` call of the `chainnotifier` RPC sub-server 
 [now optionally supports returning the entire block that confirmed the 
 transaction](https://github.com/lightningnetwork/lnd/pull/6730).

* [Add `macaroon_root_key` field to
  `InitWalletRequest`](https://github.com/lightningnetwork/lnd/pull/6457) to
  allow specifying a root key for macaroons during wallet init rather than
  having lnd randomly generate one for you.

* [A new `SignedInputs`](https://github.com/lightningnetwork/lnd/pull/6771)
  field is added to `SignPsbtResponse` that returns the indices of inputs
  that were signed by our wallet. Prior to this change `SignPsbt` didn't
  indicate whether the Psbt held any inputs for our wallet to sign.

* [Add list addresses RPC](https://github.com/lightningnetwork/lnd/pull/6596).

* Add [TrackPayments](https://github.com/lightningnetwork/lnd/pull/6335)
  method to the RPC to allow subscribing to updates from any inflight payment.
  Similar to TrackPaymentV2, but for any inflight payment.

* [Catch and throw an error](https://github.com/lightningnetwork/lnd/pull/6945)
  during `openchannel` if the local funding amount given is zero.

* [Extend](https://github.com/lightningnetwork/lnd/pull/6831) the HTLC
  interceptor server implementation with watchdog functionality to cancel back
  HTLCs for which an interceptor client does not provide a resolution in time.
  If an HTLC expires, the counterparty will claim it back on-chain and the
  receiver will lose it. Therefore the receiver can just as well fail off-chain
  a few blocks before so that the channel is saved.

* [Make remote channel reserve amount configurable for 
  `openchannel`](https://github.com/lightningnetwork/lnd/pull/6956)

* [`ForwardingHistory` ](https://github.com/lightningnetwork/lnd/pull/7001) now
  enriches each forwarding event with inbound and outbound peer alias names if
  the new flag `PeerAliasLookup` in `ForwardingHistoryRequest` is set to true.
  [`lncli fwdinghistory` ](https://github.com/lightningnetwork/lnd/pull/7083)
  enables this feature by default but adds a new flag `skip_peer_alias_lookup`
  to skip the lookup.

* The graph lookups method `DescribeGraph`, `GetNodeInfo` and `GetChanInfo` now
  [expose tlv data](https://github.com/lightningnetwork/lnd/pull/7085) that is
  broadcast over the gossip network.

* [Add new HTLC notifier event and lookup
  RPC](https://github.com/lightningnetwork/lnd/pull/6517) for the final
  settlement of incoming HTLCs. This allows applications to wait for the HTLC to
  actually disappear from all valid commitment transactions, rather than assume
  that it will. With the new extensions, situations can be avoided where the
  application considers an HTLC settled, but in reality the HTLC has timed out.

  Final resolution data will only be available for htlcs that are resolved
  after upgrading lnd.

* Zero-amount private invoices [now provide hop
  hints](https://github.com/lightningnetwork/lnd/pull/7082), up to `maxHopHints`
  (20 currently).

* [Add `creation_date_start` and `creation_date_end` filter fields to
  `ListInvoiceRequest` and
  `ListPaymentsRequest`](https://github.com/lightningnetwork/lnd/pull/7159).

* [Add chainkit RPC endpoints](https://github.com/lightningnetwork/lnd/pull/7197):
  GetBlock, GetBestBlock, GetBlockHash. These endpoints provide access to chain
  block data.

* [`QueryProbabiltiy` is deprecated. Internal mission control state can be 
  obtained via `QueryMissionControl`.](
  https://github.com/lightningnetwork/lnd/pull/6857)

## Wallet

* [Allows Taproot public keys and tap scripts to be imported as watch-only
  addresses into the internal
  wallet](https://github.com/lightningnetwork/lnd/pull/6775). NOTE that funding
  PSBTs from imported tap scripts is not currently possible.

* [The wallet birthday is now used properly when creating a watch-only wallet
  to avoid scanning the whole
  chain](https://github.com/lightningnetwork/lnd/pull/7056).

* [The PSBT output information for change outputs is now properly added when
  funding a PSBT through
  `FundPsbt`](https://github.com/lightningnetwork/lnd/pull/7209).

* [Fix the issue of ghost UTXOs not being detected as spent if they were created
  with an external tool](https://github.com/lightningnetwork/lnd/pull/7243).
  
## Routing

* Experimental support for [inbound routing
  fees](https://github.com/lightningnetwork/lnd/pull/6703) is added. This allows
  node operators to require senders to pay an inbound fee for forwards and
  payments. It is recommended to only use negative fees (an inbound "discount")
  initially to keep the channels open for senders that do not recognize inbound
  fees. In this release, no send support for pathfinding and route building is
  added yet. We first want to learn more about the impact that inbound fees have
  on the routing economy.

## Build

[The project has updated to Go
1.19](https://github.com/lightningnetwork/lnd/pull/6795)! Go 1.18 is now the
minimum version needed to build the project.

[The minimum recommended version of the Go 1.19.x series is 1.19.2 because
1.19.1 contained a bug that affected lnd and resulted in a
crash](https://github.com/lightningnetwork/lnd/pull/7019).

[Use Go's `runtime/debug` package to get information about the build](
https://github.com/lightningnetwork/lnd/pull/6963/)

[A wire parsing bug has been fixed that would cause lnd to be unable _decode_
certain large transactions](https://github.com/lightningnetwork/lnd/pull/7100).

## Misc

* [Fixed a bug where the Switch did not reforward settles or fails for
  waiting-close channels](https://github.com/lightningnetwork/lnd/pull/6789)

* [Fixed a flake in the TestChannelLinkCancelFullCommitment unit
  test](https://github.com/lightningnetwork/lnd/pull/6792).

* [Fixed error typo](https://github.com/lightningnetwork/lnd/pull/6659).

* [The macaroon key store implementation was refactored to be more generally
  usable](https://github.com/lightningnetwork/lnd/pull/6509).

* [Fixed a bug where cookie authentication with Tor would fail if the cookie
  path contained spaces](https://github.com/lightningnetwork/lnd/pull/6829).
  [With the module updated](https://github.com/lightningnetwork/lnd/pull/6836),
  `lnd` now parses Tor control port messages correctly.

* [Add option to encrypt Tor private 
  key](https://github.com/lightningnetwork/lnd/pull/6500), and [update the Tor
  module](https://github.com/lightningnetwork/lnd/pull/6526) to pave the way for
  this functionality.

* [Fixed potential data race on funding manager
  restart](https://github.com/lightningnetwork/lnd/pull/6929).

* [Fixed a flake in the TestBlockCacheMutexes unit
  test](https://github.com/lightningnetwork/lnd/pull/7029).

* [Create a helper function to wait for peer to come
  online](https://github.com/lightningnetwork/lnd/pull/6931).

* [Stop handling peer warning messages as errors](https://github.com/lightningnetwork/lnd/pull/6840)

* [Stop sending a synchronizing error on the wire when out of
  sync](https://github.com/lightningnetwork/lnd/pull/7039).

* [Update cert module](https://github.com/lightningnetwork/lnd/pull/6573) to
  allow a way to update the tls certificate without restarting lnd.

* [Fixed a bug where paying an invoice with a malformed route hint triggers a
  never-ending retry loop](https://github.com/lightningnetwork/lnd/pull/6766)

* [Migrated from go-fuzz to Go 1.18's new standard fuzz testing
  library](https://github.com/lightningnetwork/lnd/pull/7127). [Updated build
  and documentation to reflect
  this](https://github.com/lightningnetwork/lnd/pull/7142).

* [Added missing wire tests for Warning
  message](https://github.com/lightningnetwork/lnd/pull/7143).

* [The description for the `--gossip.pinned-syncers` flag was fixed to explain
  that multiple peers can be specified by using the flag multiple times instead
  of using a comma separated list of
  values](https://github.com/lightningnetwork/lnd/pull/7207).

* [Updated several tlv stream-decoding callsites to use tlv/v1.1.0 P2P variants
  for untrusted input.](https://github.com/lightningnetwork/lnd/pull/7227)

* [Prevent nil pointer dereference during funding manager 
  test](https://github.com/lightningnetwork/lnd/pull/7268)
  
* Fixed a [failure message parsing bug](https://github.com/lightningnetwork/lnd/pull/7262)
  that caused additional failure message data to be interpreted as being part of
  a channel update.

## `lncli`

* [Add an `insecure` flag to skip tls auth as well as a `metadata` string slice
  flag](https://github.com/lightningnetwork/lnd/pull/6818) that allows the
  caller to specify key-value string pairs that should be appended to the
  outgoing context.

* [Fix](https://github.com/lightningnetwork/lnd/pull/6858) command line argument
  parsing for `lncli sendpayment`.

* [Fix](https://github.com/lightningnetwork/lnd/pull/6875) mapslice cap out of 
  range error that occurs if the number of profiles is zero.

* [A new config option, `batchwindowduration` has been added to
  `sweeper`](https://github.com/lightningnetwork/lnd/pull/6868) to allow
  customize sweeper batch duration.

* [Add `base_fee_msat` and `fee_rate_ppm` flags to
  `openchannel`](https://github.com/lightningnetwork/lnd/pull/6753) requests 
  so that the user can specify fees during channel creation time in addition
  to the default configuration.

* [Sleep for 10ms when funding locked message is not
  received](https://github.com/lightningnetwork/lnd/pull/7126) to avoid CPU
  spike.

* [A new config option, `mailboxdeliverytimeout` has been added to
  `htlcswitch`](https://github.com/lightningnetwork/lnd/pull/7066).

* [Label the openchannel tx first before notifying the channel open
  event.](https://github.com/lightningnetwork/lnd/pull/7158) 

* [Add check for `pay_req` argument in `sendpayment` and `decodepayreq`
  commands to trim "lightning:" prefix before processing the request](
  https://github.com/lightningnetwork/lnd/pull/7150). Invoices may be prefixed
  with "lightning:" on mobile and web apps and it's likely for users to copy
  the invoice payment request together with the prefix, which throws checksum
  error when pasting it to the CLI.

* [Allow lncli to read binary PSBTs](https://github.com/lightningnetwork/lnd/pull/7122)
  from a file during PSBT channel funding flow to comply with [BIP 174](https://github.com/bitcoin/bips/blob/master/bip-0174.mediawiki#specification)

* [Add interface to chainkit RPC](https://github.com/lightningnetwork/lnd/pull/7197).
  This addition consists of the `chain` subcommand group: `getblock`,
  `getblockhash`, and `getbestblock`. These commands provide access to chain
  block data.

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/7186) that might
  lead to channel updates being missed, causing channel graph being incomplete.

## Code Health

* [test: use `T.TempDir` to create temporary test
  directory](https://github.com/lightningnetwork/lnd/pull/6710)

* [The `tlv` package now allows decoding records larger than 65535 bytes. The
  caller is expected to know that doing so with untrusted input is
  unsafe.](https://github.com/lightningnetwork/lnd/pull/6779)
 
* [test: replace defer cleanup with
  `t.Cleanup`](https://github.com/lightningnetwork/lnd/pull/6864).

* [test: fix loop variables being accessed in
  closures](https://github.com/lightningnetwork/lnd/pull/7032).

* [Fix loop and other temporary variables being accessed in
  goroutines](https://github.com/lightningnetwork/lnd/pull/7188).

* [CI: update test coverage library
  go-acc](https://github.com/lightningnetwork/lnd/pull/7221) v0.2.6 -> v0.2.8

* Payment related code [has been
  refactored](https://github.com/lightningnetwork/lnd/pull/7174) to allow the
  usage of new payment statuses.
 
## Watchtowers

* [Create a towerID-to-sessionID index in the wtclient DB to improve the 
  speed of listing sessions for a particular tower ID](
  https://github.com/lightningnetwork/lnd/pull/6972). This PR also ensures a 
  closer coupling of Towers and Sessions and ensures that a session cannot be
  added if the tower it is referring to does not exist.

* [Remove `AckedUpdates` & `CommittedUpdates` from the `ClientSession`
  struct](https://github.com/lightningnetwork/lnd/pull/6928) in order to
  improve the performance of fetching a `ClientSession` from the DB.

* [Allow user to update tower address without requiring a restart. Also allow
  the removal of a tower address if the current session negotiation is not
  using the address in question](
  https://github.com/lightningnetwork/lnd/pull/7025)

## Pathfinding

* [Pathfinding takes capacity of edges into account to improve success
  probability estimation.](https://github.com/lightningnetwork/lnd/pull/6857)

### Tooling and documentation

* [The `golangci-lint` tool was updated to
  `v1.50.1`](https://github.com/lightningnetwork/lnd/pull/7173)

* [Tests in `htlcswitch` will now clean up the temporary resources they create](https://github.com/lightningnetwork/lnd/pull/6832).

* Updated the github actions to use `make fmt-check` in its [build
  process](https://github.com/lightningnetwork/lnd/pull/6853).

* Database related code was refactored to [allow external tools to use it more
  easily](https://github.com/lightningnetwork/lnd/pull/5561), in preparation for
  adding a data migration functionality to `lndinit`.

* [`golangci-lint` will now check new code using additional
  linters.](https://github.com/lightningnetwork/lnd/pull/7064)

* Update github actions to [check commits against the target base 
  branch](https://github.com/lightningnetwork/lnd/pull/7103) rather than just 
  using the master branch. And [skip the commit 
  check](https://github.com/lightningnetwork/lnd/pull/7114) for all non-PR 
  events.

* Fixed docker image version used in
  [`tools`](https://github.com/lightningnetwork/lnd/pull/7254).

### Integration test

The `lntest` has been
[refactored](https://github.com/lightningnetwork/lnd/pull/6759) to provide a
better testing suite for writing integration tests. A new defined structure is
implemented, please refer to
[README](https://github.com/lightningnetwork/lnd/tree/master/lntemp) for more
details. Along the way, several
PRs([6776](https://github.com/lightningnetwork/lnd/pull/6776),
[6822](https://github.com/lightningnetwork/lnd/pull/6822),
[7172](https://github.com/lightningnetwork/lnd/pull/7172),
[7242](https://github.com/lightningnetwork/lnd/pull/7242),
[7245](https://github.com/lightningnetwork/lnd/pull/7245)) have been made to
refactor the itest for code health and maintenance.

# Contributors (Alphabetical Order)

* Alejandro Pedraza
* andreihod
* Antoni Spaanderman
* Carla Kirk-Cohen
* Conner Babinchak
* cutiful
* Daniel McNally
* Elle Mouton
* ErikEk
* Eugene Siegel
* Graham Krizek
* hieblmi
* Jesse de Wit
* Joost Jager
* Jordi Montes
* lsunsi
* Matt Morehouse
* Michael Street
* Jordi Montes
* Olaoluwa Osuntokun
* Oliver Gugger
* Priyansh Rastogi
* Robyn Ffrancon
* Roei Erez
* Tommy Volk
* Yong Yu
