# Release Notes

## Bug Fixes

* [A Taproot related key tweak issue was fixed in `btcd` that affected remote
  signing setups](https://github.com/lightningnetwork/lnd/pull/7130).

* [Sleep for one second when funding locked message is not
  received](https://github.com/lightningnetwork/lnd/pull/7095) to avoid CPU
  spike.

* [The wallet birthday is now used properly when creating a watch-only wallet
  to avoid scanning the whole
  chain](https://github.com/lightningnetwork/lnd/pull/7056).

# Contributors (Alphabetical Order)

* Oliver Gugger
* Yong Yu
