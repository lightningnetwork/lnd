CREATE INDEX IF NOT EXISTS invoices_hash_idx ON invoices(hash);
CREATE INDEX IF NOT EXISTS invoices_payment_addr_idx ON invoices(payment_addr);
CREATE INDEX IF NOT EXISTS invoices_preimage_idx ON invoices(preimage);
CREATE INDEX IF NOT EXISTS invoices_settled_at_idx ON invoices(settled_at);
