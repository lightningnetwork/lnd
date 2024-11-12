-- migration_tracker is a table that keeps track of custom migrations that have
-- been run on the database. This table is used to ensure that migrations are
-- idempotent and are only run once.
CREATE TABLE IF NOT EXISTS migration_tracker (
        -- migration_id is the id of the migration.
        migration_id TEXT PRIMARY KEY,

        -- migration_time is the timestamp at which the migration was run.
        migration_time TIMESTAMP
);

-- Insert the expected migrations to the migration tracker.
INSERT INTO migration_tracker (
    migration_id,
    migration_time
) VALUES 
    ('kv-invoices-to-sql', NULL);
