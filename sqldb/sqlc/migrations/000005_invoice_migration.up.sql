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
