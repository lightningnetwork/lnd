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

## Upgrading Postgres Version

This guide describes the recommended approach for upgrading your Postgres database to a newer version. This process requires planned downtime for your LND node.

### Prerequisites

- Access to both old and new Postgres instances
- Sufficient disk space for database dumps
- Administrative access to stop/start LND

### Migration Steps

#### 1. Prepare the New Postgres Instance

Install and configure the new Postgres version on your target system. Ensure the new instance is running and accessible.

Create an empty database for LND on the new Postgres instance:

```shell
$ psql -h new-postgres-host -U postgres
postgres=# CREATE DATABASE lnddb;
postgres=# CREATE USER lnduser WITH PASSWORD 'your-password';
postgres=# GRANT ALL PRIVILEGES ON DATABASE lnddb TO lnduser;
```

#### 2. Stop LND

Before beginning the migration, stop your LND node to ensure data consistency:

```shell
$ lncli stop
```

Verify that LND has fully stopped before proceeding.

#### 3. Backup the Current Database

Create a full backup of your current database using `pg_dump`:

```shell
$ pg_dump -h old-postgres-host -U lnduser -d lnddb -F c -f lnddb_backup.dump
```

The `-F c` flag creates a custom format dump which is recommended for `pg_restore`. This backup serves as your safety net during the migration.

**Important**: Store this backup in a safe location. Without a backup, you cannot recover if something goes wrong during the migration.

#### 4. Restore to New Postgres Instance

Restore the database to your new Postgres instance:

```shell
$ pg_restore -h new-postgres-host -U lnduser -d lnddb -v --on-conflict-do-nothing --exit-on-error lnddb_backup.dump
```

The `-v` flag provides verbose output so you can monitor the restoration progress.

**Critical**: Always use the `--exit-on-error` flag during restoration. Without this flag, `pg_restore` may continue even if errors occur, which can lead to incomplete schema migration and data inconsistencies. The restore process must fail fast if any errors are encountered to ensure data integrity.

#### 5. Verify the Migration

After restoration completes, perform basic verification checks:

```shell
$ psql -h new-postgres-host -U lnduser -d lnddb
```

Check that all expected tables exist:

```sql
-- Verify key-value tables
SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE '%_kv';

-- Check row counts for a sample key-value table (e.g., channeldb_kv)
SELECT COUNT(*) FROM channeldb_kv;

-- Check row counts for native SQL tables (if applicable two example tables are
-- depicted in the following)
SELECT COUNT(*) FROM invoices;
SELECT COUNT(*) FROM graph_nodes;
```

Compare the row counts with the old database to ensure data was migrated successfully.

#### 6. Update LND Configuration

Update your LND configuration to point to the new Postgres instance:

```
[db]
db.backend=postgres
db.postgres.dsn=postgresql://lnduser:your-password@new-postgres-host:5432/lnddb
db.postgres.timeout=0
```

#### 7. Start LND and Verify

Start your LND node:

```shell
$ lnd
```

Monitor the logs during startup to ensure LND connects successfully to the new database. Verify basic functionality:

```shell
$ lncli getinfo
$ lncli listchannels
$ lncli listinvoices
```

If LND starts successfully and you can query your channels and invoices, the migration is complete.

### Troubleshooting

If LND fails to start or you encounter issues:

1. Check LND logs for database connection errors
2. Verify the connection string in your LND configuration
3. Ensure the new Postgres instance is accessible from your LND host
4. Verify that all tables were restored correctly in the new database

### Post-Migration

Once you've verified everything is working correctly:

1. Keep the backup file (`lnddb_backup.dump`) for at least a few days
2. Monitor your LND node closely for the first 24-48 hours
3. The old Postgres instance can be decommissioned once you're confident in the new setup

**Note**: There is no automated rollback procedure. If you need to revert, you must restore from your backup to the old Postgres instance and reconfigure LND.

### Replication Considerations

If you are running Postgres with replication (as discussed in the "Important note about replication" section), additional steps may be needed:

1. Ensure replication is properly configured on the new Postgres instance before starting LND
2. Verify that synchronous replication is working correctly after the migration
3. Test failover scenarios to ensure the replica has been correctly synchronized
4. Monitor replication lag to ensure replicas stay in sync with the primary

**Note**: Always consult and follow the official PostgreSQL best practices for version upgrades and database migrations.
