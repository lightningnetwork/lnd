-- invoices_hash_idx is redundant: The UNIQUE constraint on invoices(hash)
-- already creates an implicit index.
DROP INDEX IF EXISTS invoices_hash_idx;

-- invoices_payment_addr_idx is redundant: The UNIQUE constraint on
-- invoices(payment_addr) already creates an implicit index.
DROP INDEX IF EXISTS invoices_payment_addr_idx;

-- invoices_preimage_idx is useless: There are no queries that filter on 
-- preimage so there is no need to index it.
DROP INDEX IF EXISTS invoices_preimage_idx;

-- invoices_settled_at_idx is useless: settled_at is NULL for all pending
-- invoices and is never used as a filter in any query (settle_index is used
-- instead).
DROP INDEX IF EXISTS invoices_settled_at_idx;
