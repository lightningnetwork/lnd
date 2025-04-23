-- Add index on settle_index for better query performance because we are 
-- filtering by settle_index.
CREATE INDEX IF NOT EXISTS idx_invoices_settle_index ON invoices (settle_index);
