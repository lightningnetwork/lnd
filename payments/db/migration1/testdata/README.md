# Payment Migration External Testdata

This directory holds a real `channel.db` (bbolt) or `channel.sqlite` file for
testing the payments KV to SQL migration locally. You can also point the test
at an existing Postgres-backed kvdb instance.

## How to use

1. Copy your `channel.db` or `channel.sqlite` file into this folder.
2. Edit `migration_external_test.go`:

   ```go
   // Comment out this line to enable the test
   t.Skipf("skipping test meant for local debugging only")

   // Set to your database filename
   const fileName = "channel.db" // or "channel.sqlite"
   ```

3. Run the test:

   ```bash
   # For Postgres backend
   go test -v -tags="test_db_postgres" -run TestMigrationWithExternalDB
   ```

## SQLite kvdb source

To migrate from a `channel.sqlite` file, run with the `kvdb_sqlite` build
tag:

```bash
go test -v -tags="test_db_sqlite kvdb_sqlite" \
  -run TestMigrationWithExternalDB
```

## Postgres kvdb source

To migrate from an existing Postgres-backed kvdb instance, edit
`postgresKVDSN` in `migration_external_test.go` (set it non-empty), then
run with the `kvdb_postgres` build tag:

```bash
go test -v -tags="kvdb_postgres test_db_postgres" \
  -run TestMigrationWithExternalDB
```

## Notes

- The external database is opened read-only.
- The test creates a fresh SQL database for each run.
- Do not commit production data; keep the file local.
