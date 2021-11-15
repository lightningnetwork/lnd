# Postgres support in LND

With the introduction of the `kvdb` interface, LND can support multiple database
backends. One of the supported backends is Postgres. This document
describes how it can be configured.

## Building LND with postgres support

To build LND with postgres support and for your specific platform, edit https://github.com/lightningnetwork/lnd/blob/1abe1da5b3a0e914b8c0f0d837fc0c17bed74a88/make/release_flags.mk#L41 by adding the `kvdb_postgres` to the end of the line.
It is also advisable to edit https://github.com/lightningnetwork/lnd/blob/1abe1da5b3a0e914b8c0f0d837fc0c17bed74a88/make/release_flags.mk#L14 by commenting out each line, except the one regarding your destination platform.

## Configuring Postgres for LND

In order for LND to run on Postgres, an empty database should already exist. A
database can be created via the usual ways (psql, pgadmin, etc). A user with
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
db.postgres.timeout=20s
```
