# Release Notes

## `lncli`

* The `lncli wallet psbt fund` command now allows users to specify the
  [`--min_confs` flag](https://github.com/lightningnetwork/lnd/pull/7510).
 
* [Add time_lock_delta overflow check for UpdateChannelPolicy](https://github.com/lightningnetwork/lnd/pull/7350)
  that ensure `time_lock_delta` is greater or equal than `0` and less or equal than `65535`

* [Added ability to backup, verify and
  restore single channels](https://github.com/lightningnetwork/lnd/pull/7437)
  to and from a file on disk.

* [Add a `fundmax` flag to `openchannel` to allow for the allocation of all
  funds in a wallet](https://github.com/lightningnetwork/lnd/pull/6903) towards
  a new channel opening.

## Watchtowers

* [Fix Address iterator 
  panic](https://github.com/lightningnetwork/lnd/pull/7556)
* [Allow caller to filter sessions at the time of reading them from 
  disk](https://github.com/lightningnetwork/lnd/pull/7059)
* [Clean up sessions once all channels for which they have updates for are
  closed. Also start sending the `DeleteSession` message to the
  tower.](https://github.com/lightningnetwork/lnd/pull/7069)
* [Don't load exhausted sessions when not
  needed](https://github.com/lightningnetwork/lnd/pull/7405). Also add a new
  `exclude_exhausted_sessions` boolean flag to the relevant lncli wtclient
  commands.
* [Recover from StateUpdateCodeClientBehind 
  error](https://github.com/lightningnetwork/lnd/pull/7541) after data loss. 

## Build
* [ldflags were being incorrectly passed](
https://github.com/lightningnetwork/lnd/pull/7359)

## Misc

* [Return `FEE_INSUFFICIENT` before checking balance for incoming low-fee
  HTLCs.](https://github.com/lightningnetwork/lnd/pull/7490).

* Optimize script allocation size in order to save
  [memory](https://github.com/lightningnetwork/lnd/pull/7464).

## Spec

* [Add test vectors for
  option_zero_fee_htlc_tx](https://github.com/lightningnetwork/lnd/pull/7439)

## RPC

* A [debug log](https://github.com/lightningnetwork/lnd/pull/7514) has been
  added to `lnrpc` so the node operator can know whether a certain request has
  happened or not.
* [Add peer_scid_alias to the response of 
  `listchannels`](https://github.com/lightningnetwork/lnd/pull/7366)

* Message `funding_locked` [has been
  renamed](https://github.com/lightningnetwork/lnd/pull/7517) to
  `channel_ready` internally to match the specs update. This should not change
  anything for the users since the message type(36) stays unchanged, except in
  the logging all the appearance of `funding_locked` replated experssion is
  replaced with `channel_ready`.
## Bug Fixes

* [Fix a bug where lnd crashes when psbt data is not fully 
available](https://github.com/lightningnetwork/lnd/pull/7529).


# Contributors (Alphabetical Order)

* ardevd
* Elle Mouton
* hieblmi
* Oliver Gugger
* Tommy Volk
* Yong Yu
* ziggie1984