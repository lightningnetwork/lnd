-- Update the expiry for all records in the invoices table. This is needed as 
-- previously we stored raw time.Duration values which are 64 bit integers and
-- are used to express duration in nanoseconds however the intent is to store
-- invoice expiry in seconds.

-- Update the expiry to 86400 seconds (24 hours) for non-AMP invoices.
UPDATE invoices
SET expiry = 86400
WHERE is_amp = FALSE;

-- Update the expiry to 2592000 seconds (30 days) for AMP invoices
UPDATE invoices
SET expiry = 2592000
WHERE is_amp = TRUE;
