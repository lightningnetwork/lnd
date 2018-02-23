torsvc
==========

The torsvc package contains utility functions that allow for interacting
with the Tor daemon. So far, supported functions include routing all traffic
over Tor's exposed socks5 proxy and routing DNS queries over Tor (A, AAAA, SRV).
In the future more features will be added: automatic setup of v2 hidden service
functionality, control port functionality, and handling manually setup v3 hidden
services.

## Installation and Updating

```bash
$ go get -u github.com/lightningnetwork/lnd/torsvc
```
