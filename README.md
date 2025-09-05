## Lightning Network Daemon

[![Release build](https://github.com/lightningnetwork/lnd/actions/workflows/release.yaml/badge.svg)](https://github.com/lightningnetwork/lnd/actions/workflows/release.yaml)
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE)
[![Irc](https://img.shields.io/badge/chat-on%20libera-brightgreen.svg)](https://web.libera.chat/#lnd)
[![Godoc](https://godoc.org/github.com/lightningnetwork/lnd?status.svg)](https://godoc.org/github.com/lightningnetwork/lnd)
[![Go Report Card](https://goreportcard.com/badge/github.com/lightningnetwork/lnd)](https://goreportcard.com/report/github.com/lightningnetwork/lnd)
[![Gurubase](https://img.shields.io/badge/Gurubase-Ask%20lnd%20Guru-006BFF)](https://gurubase.io/g/lnd)

<img src="logo.png">

The Lightning Network Daemon (`lnd`) - is a complete implementation of a
[Lightning Network](https://lightning.network) node.  `lnd` has several pluggable back-end
chain services including [`btcd`](https://github.com/btcsuite/btcd) (a
full-node), [`bitcoind`](https://github.com/bitcoin/bitcoin), and
[`neutrino`](https://github.com/lightninglabs/neutrino) (a new experimental light client). The project's codebase uses the
[btcsuite](https://github.com/btcsuite/) set of Bitcoin libraries, and also
exports a large set of isolated re-usable Lightning Network related libraries
within it.  In the current state `lnd` is capable of:
* Creating channels.
* Closing channels.
* Completely managing all channel states (including the exceptional ones!).
* Maintaining a fully authenticated+validated channel graph.
* Performing path finding within the network, passively forwarding incoming payments.
* Sending outgoing [onion-encrypted payments](https://github.com/lightningnetwork/lightning-onion)
through the network.
* Updating advertised fee schedules.
* Automatic channel management ([`autopilot`](https://github.com/lightningnetwork/lnd/tree/master/autopilot)).

## Lightning Network Specification Compliance
`lnd` _fully_ conforms to the [Lightning Network specification
(BOLTs)](https://github.com/lightningnetwork/lightning-rfc). BOLT stands for:
Basis of Lightning Technology. The specifications are currently being drafted
by several groups of implementers based around the world including the
developers of `lnd`. The set of specification documents as well as our
implementation of the specification are still a work-in-progress. With that
said, the current status of `lnd`'s BOLT compliance is:

  - [X] BOLT 1: Base Protocol
  - [X] BOLT 2: Peer Protocol for Channel Management
  - [X] BOLT 3: Bitcoin Transaction and Script Formats
  - [X] BOLT 4: Onion Routing Protocol
  - [X] BOLT 5: Recommendations for On-chain Transaction Handling
  - [X] BOLT 7: P2P Node and Channel Discovery
  - [X] BOLT 8: Encrypted and Authenticated Transport
  - [X] BOLT 9: Assigned Feature Flags
  - [X] BOLT 10: DNS Bootstrap and Assisted Node Location
  - [X] BOLT 11: Invoice Protocol for Lightning Payments

## Developer Resources

The daemon has been designed to be as developer friendly as possible in order
to facilitate application development on top of `lnd`. Two primary RPC
interfaces are exported: an HTTP REST API, and a [gRPC](https://grpc.io/)
service. The exported APIs are not yet stable, so be warned: they may change
drastically in the near future.

An automatically generated set of documentation for the RPC APIs can be found
at [api.lightning.community](https://api.lightning.community). A set of developer
resources including guides, articles, example applications and community resources can be found at:
[docs.lightning.engineering](https://docs.lightning.engineering).

Finally, we also have an active
[Slack](https://lightning.engineering/slack.html) where protocol developers, application developers, testers and users gather to
discuss various aspects of `lnd` and also Lightning in general.

First-time contributors are [highly encouraged to start with code review
first](docs/review.md), before creating their own Pull Requests.

## Installation
  In order to build from source, please see [the installation
  instructions](docs/INSTALL.md).

## Docker
  To run lnd from Docker, please see the main [Docker instructions](docs/DOCKER.md)

## IRC
  * irc.libera.chat
  * channel #lnd
  * [webchat](https://web.libera.chat/#lnd)

## Safety

When operating a mainnet `lnd` node, please refer to our [operational safety
guidelines](docs/safety.md). It is important to note that `lnd` is still
**beta** software and that ignoring these operational guidelines can lead to
loss of funds.

## Security

The developers of `lnd` take security _very_ seriously. The disclosure of
security vulnerabilities helps us secure the health of `lnd`, privacy of our
users, and also the health of the Lightning Network as a whole.  If you find
any issues regarding security or privacy, please disclose the information
responsibly by sending an email to security at lightning dot engineering,
preferably encrypted using our designated PGP key
(`91FE464CD75101DA6B6BAB60555C6465E5BCB3AF`) which can be found
[here](https://gist.githubusercontent.com/Roasbeef/6fb5b52886183239e4aa558f83d085d3/raw/5fa96010af201628bcfa61e9309d9b13d23d220f/security@lightning.engineering).

## Further reading
* [Step-by-step send payment guide with docker](https://github.com/lightningnetwork/lnd/tree/master/docker)
* [Contribution guide](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md)
