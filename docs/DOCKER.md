# Docker Instructions

## Development/testing

For development or testing, or to spin up a `btcd` backend alongside `lnd`,
check out the documentation at [docker/README.md](docker/README.md).

## Production

To use Docker in a production environment, you can run `lnd` by first creating
a Docker container, substituting the appropriate environment variables for the
configuration you want in [environment variables]:

```
docker create -e [environment variables] --name=lnd lightningnetwork/lnd
```

Then, just start the container:

```
docker start lnd
```

## Example

Here is an example testnet `lnd` that uses Neutrino:

```
docker create --name testnet-neutrino lightningnetwork/lnd --bitcoin.active --bitcoin.testnet --bitcoin.node=neutrino --neutrino.connect=faucet.lightning.community
```

Alternatively, you can specify options via environment variables:

```
docker create --name=testnet-neutrino -e BITCOIN_ACTIVE=1 -e BITCOIN_TESTNET=1 -e BITCOIN_NODE=neutrino -e NEUTRINO_CONNECT=faucet.lightning.community lightningnetwork/lnd
```


Start the container:

```
docker start lnd
```

Create a wallet (and write down the seed):

```
$ docker exec -it testnet-neutrino lncli create
```

Confirm `lnd` has begun to synchronize:

```
$ docker logs lnd
[snipped]
2018-05-01 02:28:01.201 [INF] RPCS: RPC server listening on 127.0.0.1:10009
2018-05-01 02:28:01.201 [INF] LTND: Waiting for chain backend to finish sync, start_height=2546
2018-05-01 02:28:01.201 [INF] RPCS: gRPC proxy started at 127.0.0.1:8080
2018-05-01 02:28:08.999 [INF] LNWL: Caught up to height 10000
2018-05-01 02:28:09.872 [INF] BTCN: Processed 10547 blocks in the last 10.23s (height 10547, 2012-05-28 05:02:32 +0000 UTC)
```

This is a simple example, it is possible to use environment
variables or commandline options to expose RPC ports, add additional chains, and more.
