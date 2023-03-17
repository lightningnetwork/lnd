# Release Notes

## `lncli`

* The `lncli wallet psbt fund` command now allows users to specify the
  [`--min_confs` flag](https://github.com/lightningnetwork/lnd/pull/7510).

## Watchtowers

* [Allow caller to filter sessions at the time of reading them from 
  disk](https://github.com/lightningnetwork/lnd/pull/7059)

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

# Contributors (Alphabetical Order)

* Elle Mouton
* Oliver Gugger
* Tommy Volk
* Yong Yu
