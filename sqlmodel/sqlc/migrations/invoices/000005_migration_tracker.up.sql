-- The migration_tracker table keeps track of migrations that have been applied
-- to the database. This table ensures that migrations are idempotent and are
-- only run once. It tracks a global database version that encompasses both
-- schema migrations handled by golang-migrate and custom in-code migrations
-- for more complex data conversions that cannot be expressed in pure SQL.
CREATE TABLE IF NOT EXISTS migration_tracker (
        -- version is the global version of the migration. Note that we
        -- intentionally don't set it as PRIMARY KEY as it'd auto increment on
        -- SQLite and our sqlc workflow will replace it with an auto
        -- incrementing SERIAL on Postgres too. UNIQUE achieves the same effect
        -- without the auto increment.
        version INTEGER UNIQUE NOT NULL,

        -- migration_time is the timestamp at which the migration was run.
        migration_time TIMESTAMP NOT NULL
);

