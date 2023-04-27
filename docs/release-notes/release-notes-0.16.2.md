# Release Notes

## Wallet

[The mempool scanning logic no longer blocks start
up](https://github.com/lightningnetwork/lnd/pull/7641). The default polling
interval has also been increased to 60 seconds.

## Bug Fixes

[A panic has been fixed](https://github.com/lightningnetwork/lnd/pull/7637) for
neutrino nodes related to sweeper transaction replacement.
