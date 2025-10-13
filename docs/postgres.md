# Postgres support in LND

With the introduction of the `kvdb` interface, LND can support multiple database
backends. One of the supported backends is Postgres. This document
describes how it can be configured.

## Building LND with postgres support

Since `lnd v0.14.1-beta` the necessary build tags to enable postgres support are
already enabled by default. The default release binaries or docker images can
be used. To build from source, simply run:

```shell
$  make install
```

## Configuring Postgres for LND

In order for LND to run on Postgres, an empty database should already exist. A
database can be created via the usual ways (psql, pgadmin, etc.). A user with
access to this database is also required.

Creation of a schema and the tables is handled by LND automatically.

## Configuring LND for Postgres

LND is configured for Postgres through the following configuration options:

* `db.backend=postgres` to select the Postgres backend.
* `db.postgres.dsn=...` to set the database connection string that includes
  database, user and password.
* `db.postgres.timeout=...` to set the connection timeout. If not set, no
  timeout applies.

Example as follows:
```
[db]
db.backend=postgres
db.postgres.dsn=postgresql://dbuser:dbpass@127.0.0.1:5432/dbname
db.postgres.timeout=0
```
Connection timeout is disabled, to account for situations where the database
might be slow for unexpected reasons.

## Important note about replication

In case a replication architecture is planned, streaming replication should be avoided, as the master does not verify the replica is indeed identical, but it will only forward the edits queue, and let the slave catch up autonomously; synchronous mode, albeit slower, is paramount for `lnd` data integrity across the copies, as it will finalize writes only after the slave confirmed successful replication.

## What is in the database?

The Postgres database is a hybrid of key-value store and native SQL tables. The architecture originated from migrating the Bolt DB key-value store into a rebuilt Postgres key-value store, but has since evolved to include native SQL components such as invoices and the graph. The database will continue to be gradually updated until all stores are implemented in native SQL.

The key-value tables maintain the schema: `key`, `value`, `parent_id`, `id`, `sequence`. Some values are TLV-encoded while others are not.

List of key-value tables/relations:

```
              List of relations
 Schema |       Name       | Type  |  Owner
--------+------------------+-------+----------
 public | channeldb_kv     | table | lndadmin
 public | decayedlogdb_kv  | table | lndadmin
 public | macaroondb_kv    | table | lndadmin
 public | towerclientdb_kv | table | lndadmin
 public | towerserverdb_kv | table | lndadmin
 public | walletdb_kv      | table | lndadmin
```

Native SQL tables include invoices and graph data, representing the ongoing migration toward a fully native SQL implementation.
