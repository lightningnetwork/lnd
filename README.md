# `lnd` - the Lightning Network Daemon

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)]
(https://travis-ci.org/lightningnetwork/lnd) 
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)]


`lnd` is a complete implementation of a node on the [Lightning
Network](lightning.network). `lnd` is currently deployed on the Bitcoin Test
Network (testnet4) and utilizes an upcoming upgrade to Bitcoin: Segregated
Witness. The project's codebase uses the
[btcsuite](https://github.com/btcsuite/) set of Bitcoin libraries, and is
currently dependant on [btcd](https://github.com/btcsuite/btcd).

The Lightning Network Daemon (`lnd`) is capable of: creating channels, closing
channels, completely managing all channel states (including the exceptional
ones!), maintaining a fully authenticated+validated channel graph, performing
path finding within the network, passively forwarding incoming payments, and
sending outgoing [onion-encrypted
payments](https://github.com/lightningnetwork/lightning-onion) through the
network.

## Lightning Network Specification Compliance

`lnd` doesn't yet _fully_ conform to the [Lightning Network specification
(BOLT's)](https://github.com/lightningnetwork/lightning-rfc). BOLT stands for:
Basic of Lightning Technologies. The specifications are currently being drafted
by several groups of implementers based around the world including the
developers of `lnd`. The set of specification documents as well as our
implementation of the specification are still a work-in-progress. With that
said, `lnd` the current status of `lnd`'s BOLT compliance is:

  - [ ] BOLT 1: Base Protocol
     * `lnd` currently utilizes a distinct wire format which was created before
       the emgergence of the current draft of BOLT specifications. We don't
       have an `init` message, but we do have analogues to all the other
       defined message types.
  - [ ] BOLT 2: Peer Protocol for Channel Management
     * `lnd` implements all the functionality defined within the document,
       however we currently use a different set of wire messages. Additionally,
       `lnd` uses a distinct commitment update state-machine and doesn't yet
       support dynamically updating commitment fees.
  - [ ] BOLT 3: Bitcoin Transaction and Script Formats
     * `lnd` currently uses a commitment design from a prior iteration of the
       protocol. Revocation secret generation is handled by `elkrem` and our
       scripts are slightly different.
  - [X] BOLT 4: Onion Routing Protocol
  - [X] BOLT 5: Recommendations for On-chain Transaction Handling
  - [X] BOLT 7: P2P Node and Channel Discovery
  - [X] BOLT 8: Encrypted and Authenticated Transport

## Installation

* In order to build form source, the following build dependencies are required:
  * **Go 1.5 or 1.6**

    Installation instructions can be found here: http://golang.org/doc/install.
    It is recommended to add `$GOPATH/bin` to your `PATH` at this point.
    **Note:** If you are building with Go 1.5, then you'll need to enable the
    vendor experiment by setting the `GO15VENDOREXPERIMENT` environment
    variable to `1`. If you're using Go 1.6 or later, then it is safe to skip
    this step.

  * **Glide**

    This project uses `Glide` to manage dependencies as well as to provide *reproducible builds*.
    To install `Glide`, execute the following command (assumes you already have Go properly installed):

      `$ go get -u github.com/Masterminds/glide`
  * **btcd**

    This project currently requires `btcd` with segwit support, which is not
    yet merged into the master branch. Instead,
    [roasbeef](https://github.com/roasbeef/btcd) maintains a fork with his
    segwit implementation applied. To install, please see [the installation
    instructions](docs/INSTALL.md).

With the preliminary steps completed, to install `lnd`, `lncli`, and all
related dependencies run the following commands:

```
$ git clone https://github.com/lightningnetwork/lnd $GOPATH/src/github.com/lightningnetwork/lnd
$ cd $GOPATH/src/github.com/lightningnetwork/lnd
$ glide install
$ go install . ./cmd/...
```

## Updating
To update your version of `lnd` to the latest version run the following
commands:
```
$ cd $GOPATH/src/github.com/lightningnetwork/lnd
$ git pull && glide install
$ go install . ./cmd/...
```

## IRC
  * irc.freenode.net
  * channel #lnd
  * [webchat](https://webchat.freenode.net/?channels=lnd)

## License
`lnd` is licensed under the [MIT license](https://github.com/lightningnetwork/lnd/blob/master/LICENSE).
