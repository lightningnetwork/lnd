-- invoice_payment_hashes table contains the hash of the invoices. This table
-- is used during KV to SQL invoice migration as in our KV representation we
-- don't have a mapping from hash to add index.
CREATE TABLE IF NOT EXISTS invoice_payment_hashes (
     -- id represents is the key of the invoice in the KV store.
    id INTEGER PRIMARY KEY,

    -- add_index is the KV add index of the invoice.
    add_index BIGINT NOT NULL,

    -- hash is the payment hash for this invoice.
    hash BLOB
);

-- Create an indexes on the add_index and hash columns to speed up lookups.
CREATE INDEX IF NOT EXISTS invoice_payment_hashes_add_index_idx ON invoice_payment_hashes(add_index);
CREATE INDEX IF NOT EXISTS invoice_payment_hashes_hash_idx ON invoice_payment_hashes(hash);
