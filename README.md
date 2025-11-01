x402BTC Lightning Network










<img src="logo.png">

The x402BTC Lightning Network Daemon (x402lnd) is a complete implementation of a
Lightning Network
 node for the x402BTC ecosystem.
x402lnd includes several pluggable back-end chain services such as
btcd
 (full node),
bitcoind
, and
neutrino
 (experimental light client).

The project leverages the btcsuite
 Bitcoin libraries and exports
a large set of reusable Lightning Network-related modules.

x402lnd currently supports:

Creating payment channels

Closing channels

Full channel state management (including exception handling)

Maintaining an authenticated and validated channel graph

Network path finding and passive payment forwarding

Sending outgoing onion-encrypted payments

Updating advertised fee schedules

Automatic channel management (autopilot
)

Lightning Network Specification Compliance

x402lnd fully conforms to the Lightning Network specification (BOLTs)
.
BOLT stands for Basis of Lightning Technology. The specifications are a collaborative effort
among multiple implementations, including x402lnd.

Current BOLT compliance:

 BOLT 1: Base Protocol

 BOLT 2: Peer Protocol for Channel Management

 BOLT 3: Bitcoin Transaction and Script Formats

 BOLT 4: Onion Routing Protocol

 BOLT 5: Recommendations for On-chain Transaction Handling

 BOLT 7: P2P Node and Channel Discovery

 BOLT 8: Encrypted and Authenticated Transport

 BOLT 9: Assigned Feature Flags

 BOLT 10: DNS Bootstrap and Assisted Node Location

 BOLT 11: Invoice Protocol for Lightning Payments

Developer Resources

x402lnd is built for developers and supports application integration through
two primary RPC interfaces:

HTTP REST API

gRPC Service (gRPC
)

⚠️ The APIs are still under active development and may change significantly.

You can find auto-generated API documentation at:
👉 api.x402btc.network

Additional developer guides, tutorials, and example apps are available at:
👉 docs.x402btc.network

Community members and developers are welcome to join our active
Slack
 or the
IRC channel for collaboration and discussion.

New contributors are encouraged to start with code review guidelines

before opening pull requests.

Installation

To build from source, follow the installation guide
.

Docker

To run x402lnd in Docker, see the Docker setup guide
.

IRC

Server: irc.libera.chat

Channel: #x402lnd

Webchat: Join Here

Safety

When running a mainnet x402lnd node, review our operational safety guidelines
.
⚠️ x402lnd is still considered beta software, and failure to follow best practices may lead to loss of funds.

Security

The x402lnd team takes security very seriously.
If you discover a potential vulnerability, please report it responsibly via email to
security@x402btc.network
, preferably encrypted using our PGP key
(91FE464CD75101DA6B6BAB60555C6465E5BCB3AF), available here
.

Further Reading

Step-by-step send payment guide with Docker

Contribution guide
