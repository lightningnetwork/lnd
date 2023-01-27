# Release Notes

## `lncli`

* The `lncli wallet psbt fund` command now allows users to specify the
  [`--min_confs` flag](https://github.com/lightningnetwork/lnd/pull/7510).
 
* [Add time_lock_delta overflow check for UpdateChannelPolicy](https://github.com/lightningnetwork/lnd/pull/7350)
  that ensure `time_lock_delta` is greater or equal than `0` and less or equal than `65535`

* [Added ability to backup, verify and
  restore single channels](https://github.com/lightningnetwork/lnd/pull/7437)
  to and from a file on disk.

## Watchtowers

* [Allow caller to filter sessions at the time of reading them from 
  disk](https://github.com/lightningnetwork/lnd/pull/7059)
* [Clean up sessions once all channels for which they have updates for are
  closed. Also start sending the `DeleteSession` message to the
  tower.](https://github.com/lightningnetwork/lnd/pull/7069)
* [Don't load exhausted sessions when not
  needed](https://github.com/lightningnetwork/lnd/pull/7405). Also add a new
  `exclude_exhausted_sessions` boolean flag to the relevant lncli wtclient
  commands.

## Misc

* [Return `FEE_INSUFFICIENT` before checking balance for incoming low-fee
  HTLCs.](https://github.com/lightningnetwork/lnd/pull/7490).
 
## Spec

* [Add test vectors for
  option_zero_fee_htlc_tx](https://github.com/lightningnetwork/lnd/pull/7439)

## RPC

- A [debug log](https://github.com/lightningnetwork/lnd/pull/7514) has been
  added to `lnrpc` so the node operator can know whether a certain request has
  happened or not.
* [Add peer_scid_alias to the response of 
  `listchannels`](https://github.com/lightningnetwork/lnd/pull/7366)

# Contributors (Alphabetical Order)

* ardevd
* Elle Mouton
* Oliver Gugger
* Tommy Volk
* Yong Yu
