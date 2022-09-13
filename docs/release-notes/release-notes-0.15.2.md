# Release Notes

## RPC/REST Server

- A `POST` URL mapping [was added to the REST version of the `QueryRoutes` call
  (`POST /v1/graph/routes`)](https://github.com/lightningnetwork/lnd/pull/6926)
  to make it possible to specify `route_hints` over REST.

## Bug Fixes

* [A bug where LND wouldn't send a ChannelUpdate during a channel open has
  been fixed.](https://github.com/lightningnetwork/lnd/pull/6892)

# Contributors (Alphabetical Order)


## Performance improvements

* [Refactor hop hint selection
  algorithm](https://github.com/lightningnetwork/lnd/pull/6914)


# Contributors (Alphabetical Order)

* Eugene Siegel
* Jordi Montes
* Oliver Gugger

