# Release Notes

## Bug Fixes

* [Fixed an inaccurate log message during a compaction
  failure](https://github.com/lightningnetwork/lnd/pull/5961).

* [Fixed a bug in the Tor controller that would cause the health check to fail
  if there was more than one hidden service
  configured](https://github.com/lightningnetwork/lnd/pull/6016).

* [A bug has been fixed in channeldb that uses the return value without checking
  the returned error first](https://github.com/lightningnetwork/lnd/pull/6012).

* [Fixes a bug that would cause lnd to be unable to start if anchors was
  disabled](https://github.com/lightningnetwork/lnd/pull/6007).

* [Fixed a bug that would cause nodes with older channels to be unable to start
  up](https://github.com/lightningnetwork/lnd/pull/6003).

* [Addresses an issue with explicit channel type negotiation that caused
  incompatibilities when opening channels with the latest versions of
  c-lightning and eclair](https://github.com/lightningnetwork/lnd/pull/6026).

* [Ensure that if a user specifies explicit channel funding on the API level,
  then it can't be
  downgraded](https://github.com/lightningnetwork/lnd/pull/6027).

# Contributors (Alphabetical Order)

* Jamie Turley
* nayuta-ueno
* Olaoluwa Osuntokun
* Oliver Gugger
