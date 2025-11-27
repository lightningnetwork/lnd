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

Moreover for particular kv tables we also add the option to access the
tables via a global lock (single wirter). This is a temorpary measure until 
these particular tables have a native sql schema. This helps to mitigate
resource exhaustion in case LND experiencing high concurrent load:

* `db.postgres.walletdb-with-global-lock=true` to run LND with a single writer
  for the walletdb_kv table (default is true).
* `db.postgres.channeldb-with-global-lock=false` to run the channeldb_kv table
  with a single writer (default is false).

## Important note about replication

In case a replication architecture is planned, streaming replication should be avoided, as the master does not verify the replica is indeed identical, but it will only forward the edits queue, and let the slave catch up autonomously; synchronous mode, albeit slower, is paramount for `lnd` data integrity across the copies, as it will finalize writes only after the slave confirmed successful replication.

## What is in the database?

At present, the Postgres Database functions as a Key-Value Store, much as Bolt DB does. Some values are TLV-encoded while others are not. More schema will be introduced over time. At present the schema for each table/relation is simply: `key`, `value`, `parent_id`, `id`, `sequence`.

List of tables/relations:

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

Notably, Invoice DB is maintained separately alongside the LND node.
