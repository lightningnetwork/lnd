# Release Notes

## Peer to Peer Protocol

Scripts received as part of an upfront shutdown script [are now properly
sanitized](https://github.com/lightningnetwork/lnd/pull/5369) to ensure
widespread relay of potential cooperative channel closures.

## Wallet Unlocking

[A new flag has been added](https://github.com/lightningnetwork/lnd/pull/5457)
to permit users to set the new password file config while at the same time,
allowing `lnd` to start up and accept new wallet initialization/creation via the
RPC interface. The new wallet-unlock-allow-create option instructs lnd to not
fail if no wallet exists yet but instead spin up its unlocker RPC as it would
without the wallet-unlock-password-file being present.  This is not recommended
for auto-provisioned or high-security systems because the wallet creation RPC
is unauthenticated and an attacker could inject a seed while lnd is in that
state.

## RPC Server

[The `Shutdown` command will now return an
error](https://github.com/lightningnetwork/lnd/pull/5364) if one attempts to
call the command while `lnd` is rescanning.

New clients connecting/disconnecting to the transaction subscription stream
[are now logged](https://github.com/lightningnetwork/lnd/pull/5358).

[The `MinConfs` param is now properly examined if the `SendAll` param is
set](https://github.com/lightningnetwork/lnd/pull/5200) for the `SendCoins` RPC
call.

[The `abandonchannel` RPC call can now be used without the `dev` build
tag](https://github.com/lightningnetwork/lnd/pull/5335). A new flag
`--i_know_what_i_am_doing` must be specified when using the call without the
`dev` build tag active.

## Integration Test Improvements

[A bug has been fixed in the `testChannelForceClosure`
test](https://github.com/lightningnetwork/lnd/pull/5348) that would cause the
test to assert the wrong balance (the miner fee wasn't accounted for).

A bug has been [fixed](https://github.com/lightningnetwork/lnd/pull/5674) in 
the `lntest` package that prevented multiple test harnesses to be created from 
the same process.

## Forwarding Optimizations

[Decoding onion blobs is now done in
parallel](https://github.com/lightningnetwork/lnd/pull/5248) when decoding the
routing information for several HTLCs as once.

## Build System

The [`monitoring` build tag is now on by
default](https://github.com/lightningnetwork/lnd/pull/5399) for all routine
builds.

## Deadline Aware in Anchor Sweeping

Anchor sweeping is now [deadline
aware](https://github.com/lightningnetwork/lnd/pull/5148). Previously, all
anchor sweepings use a default conf target of 6, which is likely to cause
overpaying miner fees since the CLTV values of the HTLCs are far in the future.
Brought by this update, the anchor sweeping (particularly local force close)
will construct a deadline from its set of HTLCs, and use it as the conf target
when estimating miner fees. The previous default conf target 6 is now changed
to 144, and it's only used when there are no eligible HTLCs for deadline
construction.

## Bug Fixes

An optimization intended to speed up the payment critical path by
eliminating an extra RPC call [has been
reverted](https://github.com/lightningnetwork/lnd/pull/5404) as it
introduced a regression that would cause payment failure due to mismatching
heights.

[A bug has been fixed that would previously cause any HTLCs settled using the
`HltcInteceptor` API calls to always be re-forwarded on start
up](https://github.com/lightningnetwork/lnd/pull/5280).

[A bug has been fixed in the parameter parsing for the `lncli` PSBT funding
call](https://github.com/lightningnetwork/lnd/pull/5441).  Prior to this bug
fix, the sat/vb param was ignored due to a default value for `conf_target`.

[A bug has been fixed that would prevent nodes that had anchor channels opening
from accepting any new inbound "legacy"
channels](https://github.com/lightningnetwork/lnd/pull/5428).

[A bug has been fixed cause an `lncli` command to fail with on opaque error
when the referenced TLS cert file doesn't
exist](https://github.com/lightningnetwork/lnd/pull/5416).

[When `lnd` receives an HTLC failure message sourced from a private channel,
we'll now properly apply the update to the internal hop hints using during path
finding](https://github.com/lightningnetwork/lnd/pull/5332).

[A regression has been fixed that caused `keysend` payments on the "legacy" RPC
server to fail due to newly added AMP
logic](https://github.com/lightningnetwork/lnd/pull/5419).

The `ListLeases` call that was introduced in 0.13.0 also listed leases that were
already expired. Only when `lnd` is restarted, these leases were cleaned up. We
now [filter out](https://github.com/lightningnetwork/lnd/pull/5472) the expired
leases.

# Contributors (Alphabetical Order)

bluetegu 
Bjarne Magnussen 
Carla Kirk-Cohen
Carsten Otto 
ErikEk 
Eugene Seigel
de6df1re 
Joost Jager 
Juan Pablo Civile 
Linus Curiel Xanon
Olaoluwa Osuntokun 
Oliver Gugger 
Randy McMillan 
Vincent Woo 
Wilmer Paulino 
Yong Yu
