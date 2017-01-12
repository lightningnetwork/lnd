### Getting started
This document is written for people who are eager to do something with 
lightning network daemon (`lnd`). Current workflow is big because we 
recreate the whole network by ourselves, next versions will use the 
started `btcd` bitcoin node in `testnet` and `faucet` wallet from which 
you will get the bitcoins.

In current readme we decribe the steps which are needed to recreate 
following schema, and send payment from `Alice` to `Bob`.
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
 * Create Bitcoin network
 * Create `Alice` node
 * Create `Bob` node
 * Initialize `Alice` node with some amount of bitcoins
 * Open channel between `Alice` and `Bob`
 * Send payment from `Alice` to `Bob`

Start the `btcd`, create `Alice` address and mine some bitcoins.
```bash
# Create "btcd" node:
$ docker-compose up -d "btcd"

# Run "Alice" container and log into it:
$ docker-compose up -d "alice"
$ docker exec -i -t "alice" bash

# Generate new backward compatible "segwit" "alice" address:
alice$ lncli newaddress np2wkh 

# Recreate "btcd" node and set "alice" address as mining address:
$ MINING_ADDRESS=<alice_address> docker-compose up -d "btcd"

# Generate 201 block (we need at least "100 >=" blocks because of coinbase 
# block maturity and "250 >=" in order to activate segwit):
$ docker-compose run btcctl generate 250

# Check that segwit is active:
$ docker-compose run btcctl getblockchaininfo | grep -A 1 segwit
```

Now we have `btcd` node turned on and some amount of bitcoins mined on 
`Alice` address. But in order `lnd` saw them lets restart the `lnd` 
daemon.
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

# Get the identity address of "Bob" node:
bob$ lncli getinfo

# Get the IP address of "Bob" node:
$ docker inspect "bob" | grep IPAddress

# Connect "Alice" to the "Bob" node:
alice$ lncli connect <bob_identity_address>@<bob_host>:10011

# Check list of peers on "Alice" side:
alice$ lncli listpeers

# Check list of peers on "Bob" side:
bob$ lncli listpeers
```

Create `Alice<->Bob` channel.
```bash
# Open the channel with "Bob":
alice$ lncli openchannel --node_key=<bob_lightning_id> --num_confs=1 --local_amt=1000000 --push_amt=0

# Include funding transaction in block thereby open the channel:
$ docker-compose run btcctl generate 1

# Check that channel with "Bob" was created:
alice$ lncli listchannels
```

Send the payment form "Alice" to "Bob".
```
# Add invoice on "Bob" side:
bob> lncli addinvoice --value=10000

# Send payment from "Alice" to "Bob":
alice> lncli sendpayment --pay_req=<encoded_invoice>

# Check "Alice" channel balance was descreased on sent amount:
alice> lncli listchannels

# Check "Bob" channel balance was increased on sent amount:
bob> lncli listchannels
```

Now we have open channel in which we sent only one payment, lets imagine
that we sent a lots of them and we ok to close channel. Lets do it.
```
# List the "Alice" channel and retrieve "channel_point" which represent
# the opened channel:
alice> lncli listchannels

# Channel point consist of two numbers devided by ":" the first on is 
# "funding_txid" and the second one is "output_index":
alice> lncli closechannel --funding_txid=<funding_txid> --output_index=<output_index>

# Include close transaction in block thereby close the channel:
$ docker-compose run btcctl generate 1

# Check "Alice" on-chain balance was descresed on sent amount:
alice> lncli walletbalance

# Check "Bob" on-chain balance was increased on sent amount:
bob> lncli walletbalance
```


### Questions
* How to see `alice` | `bob` | `btcd` logs?
```
docker-compose logs <alice|bob|btcd> 
```