# Experimental etcd support in LND

With the recent introduction of the `kvdb` interface LND can support multiple
database backends allowing experimentation with the storage model as well as
improving robustness trough eg. replicating essential data.

Building on `kvdb` in v0.11.0 we're adding experimental [etcd](https://etcd.io)
support to LND. As this is an unstable feature heavily in development, it still
has *many* rough edges for the time being. It is therefore highly recommended to
not use LND on `etcd` in any kind of production environment especially not
on bitcoin mainnet.

## Building LND with etcd support

To create a dev build of LND with etcd support use the following command:

```shell
⛰  make tags="kvdb_etcd"
```

The important tag is the `kvdb_etcd`, without which the binary is built without
the etcd driver.

For development it is advised to set the `GOFLAGS` environment variable to 
`"-tags=test"` otherwise `gopls` won't work on code in `channeldb/kvdb/etcd`
directory.

## Running a local etcd instance for testing

To start your local etcd instance for testing run:

```shell
⛰  ./etcd \
    --auto-tls \
    --advertise-client-urls=https://127.0.0.1:2379 \
    --listen-client-urls=https://0.0.0.0:2379 \
    --max-txn-ops=16384 \
    --max-request-bytes=104857600
```

The large `max-txn-ops` and `max-request-bytes` values are currently required in
case of running LND with the full graph in etcd. Upcoming versions will split
the database to local and replicated parts and only essential parts will remain
in the replicated database, removing the requirement for these additional 
settings. These parameters have been tested to work with testnet LND.

## Configuring LND to run on etcd

To run LND with etcd, additional configuration is needed, specified either
through command line flags or in `lnd.conf`.

Sample command line:

```shell
⛰  ./lnd-debug \
    --db.backend=etcd \
    --db.etcd.host=127.0.0.1:2379 \
    --db.etcd.certfile=/home/user/etcd/bin/default.etcd/fixtures/client/cert.pem \
    --db.etcd.keyfile=/home/user/etcd/bin/default.etcd/fixtures/client/key.pem \
    --db.etcd.insecure_skip_verify
```

Sample `lnd.conf` (with other setting omitted):

```text
[db]
backend=etcd
etcd.host=127.0.0.1:2379
etcd.cerfile=/home/user/etcd/bin/default.etcd/fixtures/client/cert.pem
etcd.keyfile=/home/user/etcd/bin/default.etcd/fixtures/client/key.pem
etcd.insecure_skip_verify=true
```

Optionally users can specifiy `db.etcd.user` and `db.etcd.pass` for db user
authentication.

## Migrating existing channel.db to etcd

This is currently not supported.

## Disclaimer

As mentioned before this is an experimental feature, and with that your data
may be lost. Use at your own risk!
