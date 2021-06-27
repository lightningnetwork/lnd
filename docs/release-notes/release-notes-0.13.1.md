# Release Notes

## Peer to Peer Protocol

Scripts received as part of an upfront shutdown script [are now properly
sanitized](https://github.com/lightningnetwork/lnd/pull/5369) to ensure
widespread relay of potential cooperative channel closures.

## RPC Server

[The `Shutdown` command will now return an
error](https://github.com/lightningnetwork/lnd/pull/5364) if one attempts to
call the command while `lnd` is rescanning.

New clients connecting/disconnecting to the transaction subscription stream
[are now logged](https://github.com/lightningnetwork/lnd/pull/5358).

[The `MinConfs` param is now properly examined if the `SendAll` param is
set](https://github.com/lightningnetwork/lnd/pull/5200) for the `SendCoins` RPC
call.

## Integration Test Improvements

[A bug has been fixed in the `testChannelForceClosure`
test](https://github.com/lightningnetwork/lnd/pull/5348) that would cause the
test to assert the wrong balance (the miner fee wasn't accounted for).

## Forwarding Optimizations

[Decoding onion blobs is now done in
parallel](https://github.com/lightningnetwork/lnd/pull/5248) when decoding the
routing information for several HTLCs as once.

## Build System

The [`monitoring` build tag is now on by
default](https://github.com/lightningnetwork/lnd/pull/5399) for all routine
builds.

## Bug Fixes

An optimization intended to speed up the payment critical path by
eliminating an extra RPC call [has been
reverted](https://github.com/lightningnetwork/lnd/pull/5404) as it
introduced a regression that would cause payment failure due to mismatching
heights.

# Contributors (Alphabetical Order)
