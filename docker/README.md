### Getting started
This document is written for people who are eager to do something with 
the Lightning Network Daemon (`lnd`). This folder uses `docker-compose` to
package `lnd` and `btcd` together to make deploying the two daemons as easy as
typing a few commands. All configuration between `lnd` and `btcd` are handled
automatically by their `docker-compose` config file.


This document describes a workflow on `simnet`, a development/test network
that's similar to Bitcoin Core's `regtest` mode. In `simnet` mode blocks can be
generated as will, as the difficulty is very low. This makes it an ideal
environment for testing as one doesn't need to wait tens of minutes for blocks
to arrive in order to test channel related functionality. Additionally, it's
possible to spin up an arbitrary number of `lnd` instances within containers to
create a mini development cluster. All state is saved between instances using a
shared value.

Current workflow is big because we recreate the whole network by ourselves,
next versions will use the started `btcd` bitcoin node in `testnet` and
`faucet` wallet from which you will get the bitcoins.

In the workflow below, we describe the steps required to recreate following
topology, and send a payment from `Alice` to `Bob`.
```
+ ----- +                   + --- +
| Alice | <--- channel ---> | Bob |  <---   Bob and Alice are the lightning network daemons which 
+ ----- +                   + --- +         creates the channel and interact with each other using   
    |                          |            Bitcoin network as source of truth. 
    |                          |            
    + - - - -  - + - - - - - - +            
                 |
        + ---------------- +
        | Bitcoin network  |  <---  In current scenario for simplicity we create only one  
        + ---------------- +        "btcd" node which represents the Bitcoin network, in  
                                    real situation Alice and Bob will likely be 
                                    connected to different Bitcoin nodes.
```

#### Prerequisites
    Name  | Version 
  --------|---------
  docker-compose | 1.9.0
  docker | 1.13.0

**General workflow is following:** 

 * Create a `btcd` node running on a private `simnet`.
 * Create `Alice`, one of the `lnd` nodes in our test network.
 * Create `Bob`, the other `lnd` node in our test network.
 * Mine some blocks to send `Alice` some bitcoin.
 * Open channel between `Alice` and `Bob`.
 * Send payment from `Alice` to `Bob`.
 * Finally, close the channel between `Alice` and Bob`.

Start `btcd`, and then create an address for `Alice` that we'll directly mine
bitcoin into.
```bash
# Create "btcd" node:
$ docker-compose up -d "btcd"

# Run the "Alice" container and log into it:
$ docker-compose up -d "alice"
$ docker exec -i -t "alice" bash

# Generate a new backward compatible nested p2sh for from Alice:
alice$ lncli newaddress np2wkh 

# Recreate "btcd" node and set Alice's address as mining address:
$ MINING_ADDRESS=<alice_address> docker-compose up -d "btcd"

# Generate 201 block (we need at least "100 >=" blocks because of coinbase 
# block maturity and "250 >=" in order to activate segwit):
$ docker-compose run btcctl generate 250

# Check that segwit is active:
$ docker-compose run btcctl getblockchaininfo | grep -A 1 segwit
```

Now we have `btcd` running and some amount of bitcoins mined to the `Alice`
address. We'll need to restart `Alice` just once so she properly syncs up with
`btcd`.
```bash
# Stop "Alice" container:
$ docker-compose stop "alice"

# Start "Alice" container and log into it:
$ docker-compose up --no-recreate -d "alice"
$ docker exec -i -t "alice" bash

# Check "Alice" balance:
alice$ lncli walletbalance --witness_only=true
```

Connect `Bob` node to `Alice` node.
```bash
# Run "Bob" node and log into it:
$ docker-compose up --no-recreate -d "bob"
$ docker exec -i -t "bob" bash

# Get the identity pubkey of "Bob" node:
bob$ lncli getinfo

{
  ----> "identity_pubkey": "0290bf454f4b95baf9227801301b331e35d477c6b6e7f36a599983ae58747b3828",
	"block_height": 3949,
	"block_hash": "00000000853c9dcccf8879abb0a91f0152aed16efe68015a924156f5845016ee",
	"synced_to_chain": true,
	"testnet": false,
}

# Get the IP address of "Bob" node:
$ docker inspect "bob" | grep IPAddress

# Connect "Alice" to the "Bob" node:
alice$ lncli connect <bob_pubkey>@<bob_host>:10011

# Check list of peers on "Alice" side:
alice$ lncli listpeers
{
	"peers": [
		{
			"pub_key": "0290bf454f4b95baf9227801301b331e35d477c6b6e7f36a599983ae58747b3828",
			"peer_id": 1,
			"address": "10.0.0.125:10011",
			"bytes_sent": 3278,
			"bytes_recv": 3278
		}
	]
}

# Check list of peers on "Bob" side:
bob$ lncli listpeers
{
	"peers": [
		{
			"pub_key": "036a0c5ea35df8a528b98edf6f290b28676d51d0fe202b073fe677612a39c0aa09",
			"peer_id": 1,
			"address": "10.0.0.15:10011",
			"bytes_sent": 3278,
			"bytes_recv": 3278
		}
	]
}
```

Create the `Alice<->Bob` channel.
```bash
# Open the channel with "Bob":
alice$ lncli openchannel --node_key=<bob_lightning_id> --num_confs=1 --local_amt=1000000

# Include funding transaction in block thereby open the channel:
$ docker-compose run btcctl generate 1

# Check that channel with "Bob" was created:
alice$ lncli listchannels
{
	"channels": [
		{
			"remote_pubkey": "0290bf454f4b95baf9227801301b331e35d477c6b6e7f36a599983ae58747b3828",
			"channel_point": "7a632cde9e9e2ae4e9209591c0587bbb03254814c62e2a7fcef35ced743b0025:0",
			"chan_id": 1170330072213225472,
			"capacity": 1005000,
			"local_balance": 1000000,
		}
	]
}

```

Send the payment form "Alice" to "Bob".
```
# Add invoice on "Bob" side:
bob> lncli addinvoice --value=10000
{
        "r_hash": "<your_random_rhash_here>", 
        "pay_req": "<encoded_invoice>", 
}

# Send payment from "Alice" to "Bob":
alice> lncli sendpayment --pay_req=<encoded_invoice>

# Check "Alice"'s channel balance was decremented accordingly by the payment
# amount
alice> lncli listchannels

# Check "Bob"'s channel balance was credited with the payment amount
bob> lncli listchannels
```

Now we have open channel in which we sent only one payment, lets imagine
that we sent a lots of them and we'll now like to close the channel. Lets do
it!
```
# List the "Alice" channel and retrieve "channel_point" which represent
# the opened channel:
alice> lncli listchannels
{
	"channels": [
		{
			"remote_pubkey": "0290bf454f4b95baf9227801301b331e35d477c6b6e7f36a599983ae58747b3828",
			"channel_point": "7a632cde9e9e2ae4e9209591c0587bbb03254814c62e2a7fcef35ced743b0025:0",
			"chan_id": 1170330072213225472,
			"capacity": 1005000,
			"local_balance": 900000,
                        "remote_balance": 10000, 
		}
	]
}

# Channel point consist of two numbers separated by colon the first on is
# "funding_txid" and the second one is "output_index":
alice> lncli closechannel --funding_txid=<funding_txid> --output_index=<output_index>

# Include close transaction in block thereby close the channel:
$ docker-compose run btcctl generate 1

# Check "Alice" on-chain balance was credited by her settled amount in the channel:
alice> lncli walletbalance

# Check "Bob" on-chain balance was credited with the funds he received in the
# channel:
bob> lncli walletbalance
```


### Questions
* How to see `alice` | `bob` | `btcd` logs?
```
docker-compose logs <alice|bob|btcd> 
```
