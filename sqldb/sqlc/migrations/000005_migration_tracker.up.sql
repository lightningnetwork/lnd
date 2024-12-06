-- migration_tracker is a table that keeps track of migrations that have been
-- run on the database. It tracks a global DB version that encompasses both the
-- schema migrations handled by golang-migrate This table is used to ensure
-- that migrations are idempotent and are only run once.
CREATE TABLE IF NOT EXISTS migration_tracker (
        -- version is the global version of the migration. Note that we
        -- intentionally don't set it as PRIMARY KEY as it'd auto increment on
        -- SQLite and our sqlc workflow will replace it with an auto incementing
        -- SERIAL on Postgres too. UNIQUE achieves the same effect without the
        -- auto increment.
        version INTEGER UNIQUE,

        -- migration_time is the timestamp at which the migration was run.
        migration_time TIMESTAMP
);

