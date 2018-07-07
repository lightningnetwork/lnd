# Table of Contents
1. [Overview](#overview)
2. [Setup](#setup)

## Overview

`lnd` currently has complete support for using Lightning over
[Tor](https://www.torproject.org/). Usage of Lightning over Tor is valuable as
routing nodes no longer need to potentially expose their location via their
advertised IP address. Additionally, leaf nodes can also protect their location
by using Tor for anonymous networking to establish connections.

With widespread usage of Onion Services within the network, concerns about the
difficulty of proper NAT traversal are alleviated, as usage of Onion Services
allows nodes to accept inbound connections even if they're behind a NAT.

Before following the remainder of this documentation, you should ensure that
you already have Tor installed locally. Official instructions to install the
latest release of Tor can be found
[here](https://www.torproject.org/docs/tor-doc-unix.html.en).

**NOTE**: This documentation covers how to ensure that `lnd`'s _Lightning
protocol traffic_ is tunneled over Tor. Users must ensure that when also running
a Bitcoin full-node, that it is also proxying all traffic over Tor. If using the
`neutrino` backend for `lnd`, then it will automatically also default to Tor
usage if active within `lnd`.

## Setup

First, you'll want to run `tor` locally before starting up `lnd`. Depending on
how you installed Tor, you'll find the configuration file at
`/usr/local/etc/tor/torrc`. Here's an example configuration file that we'll be
using for the remainder of the tutorial:
```
SOCKSPort 9050
Log notice stdout
ControlPort 9051
CookieAuthentication 1
```

With the configuration file created, you'll then want to start the Tor daemon:
```
⛰  tor
Feb 05 17:02:06.501 [notice] Tor 0.3.1.8 (git-ad5027f7dc790624) running on Darwin with Libevent 2.1.8-stable, OpenSSL 1.0.2l, Zlib 1.2.8, Liblzma N/A, and Libzstd N/A.
Feb 05 17:02:06.502 [notice] Tor can't help you if you use it wrong! Learn how to be safe at https://www.torproject.org/download/download#warning
Feb 05 17:02:06.502 [notice] Read configuration file "/usr/local/etc/tor/torrc".
Feb 05 17:02:06.506 [notice] Opening Socks listener on 127.0.0.1:9050
Feb 05 17:02:06.506 [notice] Opening Control listener on 127.0.0.1:9051
```

Once the `tor` daemon has started and it has finished bootstrapping, you'll see this in the logs:
```
Feb 05 17:02:06.000 [notice] Bootstrapped 0%: Starting
Feb 05 17:02:07.000 [notice] Starting with guard context "default"
Feb 05 17:02:07.000 [notice] Bootstrapped 80%: Connecting to the Tor network
Feb 05 17:02:07.000 [notice] Bootstrapped 85%: Finishing handshake with first hop
Feb 05 17:02:08.000 [notice] Bootstrapped 90%: Establishing a Tor circuit
Feb 05 17:02:11.000 [notice] Tor has successfully opened a circuit. Looks like client functionality is working.
Feb 05 17:02:11.000 [notice] Bootstrapped 100%: Done
```

This indicates the daemon is fully bootstrapped and ready to proxy connections.
At this point, we can now start `lnd` with the relevant arguments:

```
⛰  ./lnd -h

<snip>
      --tor.socks=                                            The port that Tor's exposed SOCKS5 proxy is listening on. Using Tor allows outbound-only connections (listening will be disabled) -- NOTE
      --tor.dns=                                              The DNS server as IP:PORT that Tor will use for SRV queries - NOTE must have TCP resolution enabled
      --tor.streamisolation                                   Enable Tor stream isolation by randomizing user credentials for each connection.

```
1. We start by launching bitcoind with the following flag:

```shell
⛰  ./bitcoind --torcontrol=127.0.01:9051
```

2. Next we start lnd with the following flags:
```
⛰  ./lnd --tor.socks=9050 --tor.dns=nodes.lightning.directory
```

```
This will allow you to make all outgoing connections over Tor, but still allow
regular (clearnet) incoming connections.

## Tor Stream Isolation

Our support for Tor also has an additional privacy enhancing modified: stream
isolation. Usage of this mode means that Tor will always use  _new circuit_ for
each connection. This added features means that it's harder to correlate
connections. As otherwise, several applications using Tor might share the same
circuit.

Activating stream isolation is very straightforward, we only require the
specification of an additional argument:
```
⛰  ./lnd --tor.socks=9050 --tor.dns=nodes.lightning.directory --tor.streamisolation
```
