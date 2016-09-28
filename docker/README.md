### Generate bitcoin infrastructure and mine some bitcoin.

**Step №1**: Run `btcwallet`.
```
$ docker-compose up -d btcwallet
```

**Step №2**: Check connection with `btcwallet`.
```
$ docker-compose run --rm walletctl getinfo
```

**Step №3**: Generate new default address.
```
$ docker-compose run --rm walletctl getnewaddress default`
```

**Step №4**: Recreate `btcd` node and initialize mining address.
```
$ MINING_ADDRESS=<default_address> docker-compose up -d btcd
```

**Step №5**: Check connection with  `btcd`.
```
docker-compose run btcctl getinfo
```

**Step №6**: Generate 120 block (we need at least 100 block because of coinbase block maturity)
```
docker-compose run btcctl generate 120
```

**Step №7**: Check account balance.
```
$ docker-compose run walletctl getbalance
```

**Step №8**: Run two `lnd` nodes/containers:
```
$ docker-compose scale lnd=2
```

**Step №9**: I couldn't create `lncli` service because for now `lnd` listen RPC commands only for `localhost`. For that reason we should log into `lnd` containers. Log into `lnd` containers:
```
$ docker exec -i -t <first_lnd_container_name> bash
$ docker exec -i -t <second_lnd_container_name> bash
```

### Send bitcoins on `lnd` p2wkh address.

**Step №1**: (In first `lnd` container bash) Generate new p2pkh address. 
```
$ lncli newaddress p2pkh
```

**Step №2**: Unlock wallet.
```
$ docker-compose run --rm walletctl walletpassphrase "password" 999
```

**Step №3**: Send money from default wallet p2pkh address to lnd p2pkh address.
```
$ docker-compose run --rm walletctl sendtoaddress <p2pkh_address> <amount>
```

**Step №4**: Make transaction visible.
```
$ docker-compose run btcctl generate 1
```

**Step №5**: (In first`lnd` container bash) Generate new p2wkh address. 
```
$ lncli newaddress p2wkh
```

**Step №6**: (In first`lnd` container bash) Send from lnd p2pkh address to lnd p2wkh address.
```
$ lncli sendcoins --addr=<p2wkh_addr> --amt=<amount>
```

**Step №7**: Make transactions visible.
```
$ docker-compose run btcctl generate 1
```

**Step №8**: (In first`lnd` container bash) Check `lnd` balance.
```
$ lncli walletbalance --witness_only=true
```

### Create and use `lnd` channel.

**Step №1**: (In first`lnd` container bash) Get the lnd identity address of first node.
```
$ lncli getinfo
```

**Step №2**: Get the IP address of first node.
```
$ docker inspect <lnd_container_name> | grep IPAddress
```

**Step №3**: (In second `lnd` container bash) Connect to the first node.
```
$ lncli connect <lnd_identity_address>@<host>:10011
```

**Step №4**: (In second `lnd` container bash) Check list of peers.
```
$ lncli listpeers
```

**Step №5**: (In second `lnd` container bash) Open the channel.
```
$ lncli openchannel --peer_id=1 --num_confs=1 --local_amt=100000000
```

**Step №6**: Make funding transaction visible.
```
$ docker-compose run btcctl generate 1
```

### TODO(andrew.shvv) Add 'sendpayment' example after routing manager will be created. 
