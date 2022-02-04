# Release Notes

## Security

* [Misconfigured ZMQ
  setup now gets reported](https://github.com/lightningnetwork/lnd/pull/5710).

* [Add option to encrypt Tor private key on disk](https://github.com/lightningnetwork/lnd/pull/4458).

## `lncli`

* Add [auto-generated command-line completions](https://github.com/lightningnetwork/lnd/pull/4177) 
  for Fish shell.  

* Add [chan_point flag](https://github.com/lightningnetwork/lnd/pull/6152)
  to closechannel command.

* Add [private status](https://github.com/lightningnetwork/lnd/pull/6167)
  to pendingchannels response.

## Bug Fixes

* [Fixed an inactive invoice subscription not removed from invoice
  registry](https://github.com/lightningnetwork/lnd/pull/6053). When an invoice
  subscription is created and canceled immediately, it could be left uncleaned
  due to the cancel signal is processed before the creation. It is now properly
  handled by moving creation before deletion.   

* When the block height+delta specified by a network message is greater than
  the gossiper's best height, it will be considered as premature and ignored.
  [These premature messages are now saved into a cache and processed once the
  height has reached.](https://github.com/lightningnetwork/lnd/pull/6054)

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
  
* [Add `.vs/` folder to `.gitignore`](https://github.com/lightningnetwork/lnd/pull/6178). 

* [Chain backend healthchecks disabled for --nochainbackend mode](https://github.com/lightningnetwork/lnd/pull/6184)

## RPC Server

* [Add value to the field
  `remote_balance`](https://github.com/lightningnetwork/lnd/pull/5931) in
  `pending_force_closing_channels` under `pendingchannels` whereas before was
  empty(zero).

* [Add dev only RPC subserver and the devrpc.ImportGraph
  call](https://github.com/lightningnetwork/lnd/pull/6149)
  
* [Extend](https://github.com/lightningnetwork/lnd/pull/6177) the HTLC
  interceptor API to provide more control over failure messages. With this
  change, it allows encrypted failure messages to be returned to the sender.
  Additionally it is possible to signal a malformed htlc.

## Documentation

* Improved instructions on [how to build lnd for mobile](https://github.com/lightningnetwork/lnd/pull/6085).
* [Log force-close related messages on "info" level](https://github.com/lightningnetwork/lnd/pull/6124).

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

# Contributors (Alphabetical Order)

* 3nprob
* Andreas Schjønhaug
* asvdf
* BTCparadigm
* Carla Kirk-Cohen
* Carsten Otto
* Dan Bolser
* Daniel McNally
* ErikEk
* Graham Krizek
* henta
* Joost Jager
* Jordi Montes
* LightningHelper
* Liviu
* mateuszmp
* Naveen Srinivasan
* randymcmillan
* Rong Ou
* Thebora Kompanioni
* Torkel Rogstad
* Vsevolod Kaganovych
* Yong Yu
