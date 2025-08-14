# SQL Schema Updates in LND

This document provides guidance for adding new SQL schema migrations in the LND codebase. When adding a new SQL schema, multiple files need to be updated to ensure consistency across different build configurations and environments.

## Overview

LND uses a dual approach for database migrations:
1. **SQL Schema Migrations**: Standard SQL files handled by golang-migrate
2. **Custom Application-Level Migrations**: Go functions that perform data migration or complex logic

The migration system supports both development and production configurations through build tags.

## Required Updates Checklist

When adding a new SQL schema migration, you **MUST** update the following locations:

### 1. SQL Migration Files
Create the migration SQL files in the appropriate directory:
```
sqldb/sqlc/migrations/
├── 000XXX_your_migration_name.up.sql    # Schema changes
└── 000XXX_your_migration_name.down.sql  # Rollback changes
```

### 2. Migration Configuration
Add the migration entry to **ONE** of these files based on your target environment:

#### For Production Releases:
**File**: `sqldb/migrations.go`
```go

migrationConfig = append([]MigrationConfig{
    // ... existing migrations
    {
        Name:          "000XXX_your_migration_name",
        Version:       X,
        SchemaVersion: X,
        // MigrationFn: customMigrationFunction, // Only if custom logic needed
    },
```

#### For Development/Testing:
**File**: `sqldb/migrations_dev.go`
```go
//go:build test_db_postgres || test_db_sqlite || test_native_sql

var migrationAdditions = []MigrationConfig{
    // ... existing migrations
    {
        Name:          "000XXX_your_migration_name",
        Version:       X,
        SchemaVersion: X,
        // MigrationFn: customMigrationFunction, // Only if custom logic needed
    },
}
```

### 3. Custom Migration Functions (If Needed)
If your migration requires custom application logic (data transformation, complex operations), you need to:

**⚠️ Important**: Custom migrations do NOT have corresponding SQL files in the `sqlc/migrations/` directory. They are purely application-level migrations defined in Go code.

#### 3a. Add Custom Migration Entry to Migration Configuration

same as in section 2 without the number prefix. See the example for more information.


#### 3b. Add Migration Function Reference
Add the migration function to the appropriate config files:

**For Production**: `config_builder.go`
```go
func (d *DefaultDatabaseBuilder) BuildDatabase(
    	ctx context.Context) (*DatabaseInstances, func(), error) {

            	invoiceMig := func(tx *sqlc.Queries) error {
				err := invoices.MigrateInvoicesToSQL(
					ctx, dbs.ChanStateDB.Backend,
					dbs.ChanStateDB, tx,
					invoiceMigrationBatchSize,
				)
				if err != nil {
					return fmt.Errorf("failed to migrate "+
						"invoices to SQL: %w", err)
				}

				// Set the invoice bucket tombstone to indicate
				// that the migration has been completed.
				d.logger.Debugf("Setting invoice bucket " +
					"tombstone")

				return dbs.ChanStateDB.SetInvoiceBucketTombstone() //nolint:ll
			
```

**For Testing**: `config_test_native_sql.go`
```go
func (d *DefaultDatabaseBuilder) getSQLMigration(ctx context.Context,
    version int, kvBackend kvdb.Backend) (func(tx *sqlc.Queries) error, bool) {

    switch version {
    case X: // Use the version number from your migrationAdditions  
        return func(tx *sqlc.Queries) error {
            // Your custom migration logic here
            return nil
        }, true
    }

    return nil, false
}
```

#### 3b. Implement Migration Function
Create the actual migration function (typically in the same package that owns the data):
```go
func yourCustomMigrationFunction(tx *sqlc.Queries) error {
    // Your custom migration logic here
    // Example: data transformation, index creation, etc.
    return nil
}
```

## Important Guidelines

### Version Numbering
- **Schema Version**: Must match the SQL file number (e.g., `000008` → `SchemaVersion: 8`)
- **Migration Version**: Must be sequential and unique across all migrations
- **File Naming**: Use format `000XXX_descriptive_name.{up|down}.sql`

### Build Tags
- **Production builds** (default): Use `migrations_prod.go` and `config_prod.go`
- **Test builds**: Use `migrations_dev.go` and `config_test_native_sql.go`
- The build system automatically includes the appropriate files based on build tags

### Custom Migrations
- **No SQL files**: Custom migrations do NOT have `.up.sql` or `.down.sql` files - they are pure Go code
- Custom KV→SQL migrations should use the same `SchemaVersion` as their corresponding SQL migration
- KV migration `Version` should be higher than the SQL migration version
- Custom migrations only require config entries in `migrationAdditions` and implementation in `getSQLMigration`

## Examples

### Example 1: Simple Schema Migration - `000008_graph`

The `000008_graph` migration is a real example from the codebase:

1. **SQL files exist**:
   ```sql
   -- sqldb/sqlc/migrations/000008_graph.up.sql
   -- (Contains graph schema creation SQL)
   ```

2. **Migration config in migrations_dev.go**:
   ```go
   var migrationAdditions = []MigrationConfig{
       {
           Name:          "000008_graph",
           Version:       9,
           SchemaVersion: 8,
       },
   }
   ```

### Example 2: Schema Migration with Custom Logic - `000008_graph` + `kv_graph_migration`

This shows how `000008_graph` (SQL schema) works together with `kv_graph_migration` (custom logic):

1. **SQL files for schema**:
   ```sql
   -- sqldb/sqlc/migrations/000008_graph.up.sql
   -- Creates graph tables and indexes
   ```

2. **Schema migration in migrations_dev.go**:
   ```go
   var migrationAdditions = []MigrationConfig{
       {
           Name:          "000008_graph", 
           Version:       9,
           SchemaVersion: 8,
       },
       {
           Name:          "kv_graph_migration",
           Version:       10,
           SchemaVersion: 8, // Same schema version as 000008_graph
       },
   }
   ```

3. **Custom migration logic in config_test_native_sql.go**:
   ```go
   const graphSQLMigration = 10

   func (d *DefaultDatabaseBuilder) getSQLMigration(ctx context.Context,
       version int, kvBackend kvdb.Backend) (func(tx *sqlc.Queries) error, bool) {

       switch version {
       case graphSQLMigration: // version 10 (kv_graph_migration)
           return func(tx *sqlc.Queries) error {
               err := graphdb.MigrateGraphToSQL(ctx, cfg, kvBackend, tx)
               if err != nil {
                   return fmt.Errorf("failed to migrate graph to SQL: %w", err)
               }
               return nil
           }, true
       }

       return nil, false
   }
   ```

This pattern shows:
- Version 9: SQL schema creation (`000008_graph`)  
- Version 10: Data migration from KV to SQL (`kv_graph_migration`)


## Common Mistakes to Avoid

1. **Forgetting migration config entry**: Every SQL file must have a corresponding config entry
2. **Version number mismatch**: Schema version must match SQL file number
3. **Missing custom migration reference**: If using `MigrationFn`, must update appropriate config file
4. **Wrong build tag file**: Use prod files for releases, dev files for testing
5. **Non-sequential versions**: Migration versions must be sequential without gaps
6. **Inconsistent cross-environment**: Ensure both prod and dev configurations are updated if needed
