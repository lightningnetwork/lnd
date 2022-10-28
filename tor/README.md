tor
===

The tor package contains utility functions that allow for interacting with the
Tor daemon. So far, supported functions include:

* Routing all traffic over Tor's exposed SOCKS5 proxy.
* Routing DNS queries over Tor (A, AAAA, SRV).
* Limited Tor Control functionality (synchronous messages only). So far, this
includes:
  * Support for SAFECOOKIE, HASHEDPASSWORD, and NULL authentication methods.
  * Creating v2 and v3 onion services.

In the future, the Tor Control functionality will be extended to support v3
onion services, asynchronous messages, etc.

## Installation and Updating

```shell
$  go get -u github.com/lightningnetwork/lnd/tor
```
