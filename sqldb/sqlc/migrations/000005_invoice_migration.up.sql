-- migration_tracker is a table that keeps track of custom migrations that have
-- been run on the database. This table is used to ensure that migrations are
-- idempotent and are only run once.
CREATE TABLE IF NOT EXISTS migration_tracker (
        -- migration_id is the id of the migration.
        migration_id TEXT NOT NULL,

        -- migration_ts is the timestamp at which the migration was run.
        migration_ts TIMESTAMP,

        PRIMARY KEY (migration_id)
);

-- Insert the expected migrations to the migration tracker.
INSERT INTO migration_tracker (
    migration_id,
    migration_ts
) VALUES 
    ('kv-invoices-to-sql', NULL);

-- invoice_payment_hashes table contains the hash of the invoices. This table
-- is used during KV to SQL invoice migration as in our KV representation we
-- don't have a mapping from hash to add index.
CREATE TABLE IF NOT EXISTS invoice_payment_hashes (
    -- hash is the payment hash for this invoice. The invoice hash will always 
    -- identify that invoice.
    hash BLOB NOT NULL PRIMARY KEY,

     -- id represents is the key of the invoice in the KV store.
    id BIGINT NOT NULL,

    -- add_index is the KV add index of the invoice.
    add_index BIGINT
);
