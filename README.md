## Lightning Network Daemon

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)](https://travis-ci.org/lightningnetwork/lnd) 
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE) 
[![Irc](https://img.shields.io/badge/chat-on%20freenode-brightgreen.svg)](https://webchat.freenode.net/?channels=lnd) 
[![Godoc](https://godoc.org/github.com/lightningnetwork/lnd?status.svg)](https://godoc.org/github.com/lightningnetwork/lnd)
[![Coverage Status](https://coveralls.io/repos/github/lightningnetwork/lnd/badge.svg?branch=master)](https://coveralls.io/github/lightningnetwork/lnd?branch=master)

The Lightning Network Daemon (`lnd`) - is a complete implementation of a 
[Lightning Network](https://lightning.network) node and currently 
deployed on `testnet4` - the Bitcoin Test Network. It utilizes an 
upcoming upgrade to Bitcoin: Segregated Witness (`segwit`). The 
project's codebase uses the [btcsuite](https://github.com/btcsuite/) set
of Bitcoin libraries, and is currently dependant on [btcd](https://github.com/btcsuite/btcd). 
In the current state `lnd` is capable of: 
* creating channels
* closing channels
* completely managing all channel states (including the exceptional ones!)
* maintaining a fully authenticated+validated channel graph
* performing path finding within the network, passively forwarding 
incoming payments
* sending outgoing [onion-encrypted payments](https://github.com/lightningnetwork/lightning-onion) 
through the network

## Lightning Network Specification Compliance
`lnd` doesn't yet _fully_ conform to the [Lightning Network specification
(BOLT's)](https://github.com/lightningnetwork/lightning-rfc). BOLT stands
for: Basic of Lightning Technologies. The specifications are currently being drafted
by several groups of implementers based around the world including the
developers of `lnd`. The set of specification documents as well as our
implementation of the specification are still a work-in-progress. With that
said, `lnd` the current status of `lnd`'s BOLT compliance is:

  - [ ] BOLT 1: Base Protocol
     * `lnd` currently utilizes a distinct wire format which was created
      before the emgergence of the current draft of BOLT specifications.
      We don't have an `init` message, but we do have analogues to all 
      the other defined message types.
  - [ ] BOLT 2: Peer Protocol for Channel Management
     * `lnd` implements all the functionality defined within the 
     document, however we currently use a different set of wire messages.
     Additionally,`lnd` uses a distinct commitment update state-machine 
     and doesn't yet support dynamically updating commitment fees.
  - [ ] BOLT 3: Bitcoin Transaction and Script Formats
     * `lnd` currently uses a commitment design from a prior iteration 
     of the protocol. Revocation secret generation is handled by `elkrem`
       and our scripts are slightly different.
  - [X] BOLT 4: Onion Routing Protocol
  - [X] BOLT 5: Recommendations for On-chain Transaction Handling
  - [X] BOLT 7: P2P Node and Channel Discovery
  - [X] BOLT 8: Encrypted and Authenticated Transport

## Installation
  In order to build from source, please see [the installation
  instructions](docs/INSTALL.md).
  
## IRC
  * irc.freenode.net
  * channel #lnd
  * [webchat](https://webchat.freenode.net/?channels=lnd)

## Further reading
* [Step-by-step send payment guide with docker](https://github.com/lightningnetwork/lnd/tree/master/docker)
* [Contribution guide](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md)
