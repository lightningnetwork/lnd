# Docker Instructions

## Development/testing

For development or testing, or to spin up a `btcd` backend alongside `lnd`,
check out the documentation at [docker/README.md](../docker/README.md).

## Production

To use Docker in a production environment, you can run `lnd` by first creating
a Docker container, adding the appropriate command-line options as parameters.

```
$ docker create --name=lnd lightninglabs/lnd [command-line options]
```

Then, just start the container:

```
$ docker start lnd
```

## Volumes

A Docker volume will be created with your `.lnd` directory automatically, and will
persist through container restarts.

You can also optionally manually specify a local folder to be used as a volume:

```
$ docker create --name=lnd -v /media/lnd-docker/:/root/.lnd lightninglabs/lnd [command-line options]
```

## Example

Here is an example testnet `lnd` that uses Neutrino:

```
$ docker create --name lnd-testnet lightninglabs/lnd --bitcoin.active --bitcoin.testnet --bitcoin.node=neutrino --neutrino.connect=faucet.lightning.community
```

Start the container:

```
$ docker start lnd-testnet
```

Create a wallet (and write down the seed):

```
$ docker exec -it lnd-testnet lncli create
```

Confirm `lnd` has begun to synchronize:

```
$ docker logs lnd-testnet
[snipped]
2018-05-01 02:28:01.201 [INF] RPCS: RPC server listening on 127.0.0.1:10009
2018-05-01 02:28:01.201 [INF] LTND: Waiting for chain backend to finish sync, start_height=2546
2018-05-01 02:28:01.201 [INF] RPCS: gRPC proxy started at 127.0.0.1:8080
2018-05-01 02:28:08.999 [INF] LNWL: Caught up to height 10000
2018-05-01 02:28:09.872 [INF] BTCN: Processed 10547 blocks in the last 10.23s (height 10547, 2012-05-28 05:02:32 +0000 UTC)
```

This is a simple example, it is possible to use any command-line options necessary
to expose RPC ports, use `btcd` or `bitcoind`, or add additional chains.

## LND Development and Testing

To test the Docker production image locally, run the following from
the project root:

```
$ docker build . -t lnd:master
```

To choose a specific branch or tag instead, use the "checkout" build-arg.  For example, to build the latest commits in master:

```
$ docker build . --build-arg checkout=v0.8.0-beta -t lnd:v0.8.0-beta
```

To build the image using the most current tag:

```
$ docker build . --build-arg checkout=$(git describe --tags `git rev-list --tags --max-count=1`) -t lnd:latest-tag
```

Once the image has been built and tagged locally, start the container:

```
docker run --name=lnd-testnet -it lnd:1.0 --bitcoin.active --bitcoin.testnet --bitcoin.node=neutrino --neutrino.connect=faucet.lightning.community
```
