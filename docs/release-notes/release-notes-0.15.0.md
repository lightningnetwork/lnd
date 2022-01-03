# Release Notes

## Security

* [Misconfigured ZMQ
  setup now gets reported](https://github.com/lightningnetwork/lnd/pull/5710).

## `lncli`

* Add [auto-generated command-line completions](https://github.com/lightningnetwork/lnd/pull/4177) 
  for Fish shell.  

## Bug Fixes

* [Fixed an inactive invoice subscription not removed from invoice
  registry](https://github.com/lightningnetwork/lnd/pull/6053). When an invoice
  subscription is created and canceled immediately, it could be left uncleaned
  due to the cancel signal is processed before the creation. It is now properly
  handled by moving creation before deletion.   

## Misc

* [An example systemd service file](https://github.com/lightningnetwork/lnd/pull/6033)
  for running lnd alongside a bitcoind service is now provided in
  `contrib/init/lnd.service`.

* [Allow disabling migrations if the database backend passed to `channeldb` was
  opened in read-only mode](https://github.com/lightningnetwork/lnd/pull/6084).

* [Disable compiler optimizations](https://github.com/lightningnetwork/lnd/pull/6105)
  when building `lnd-debug` and `lncli-debug`. It helps when stepping through the code
  with a debugger like Delve.

## RPC Server

* [Add value to the field
  `remote_balance`](https://github.com/lightningnetwork/lnd/pull/5931) in
  `pending_force_closing_channels` under `pendingchannels` whereas before was
  empty(zero).

## Documentation

* Improved instructions on [how to build lnd for mobile](https://github.com/lightningnetwork/lnd/pull/6085).

## Code Health

### Code cleanup, refactor, typo fixes

* [Refactored itest to better manage contexts inside integration tests](https://github.com/lightningnetwork/lnd/pull/5756).

# Contributors (Alphabetical Order)

* Andreas Schjønhaug
* Daniel McNally
* ErikEk
* Liviu
* Torkel Rogstad
* Yong Yu
