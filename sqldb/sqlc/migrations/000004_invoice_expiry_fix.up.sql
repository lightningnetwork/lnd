-- Update the expiry to 3600 seconds (1 hour) for all records in the invoices
-- table. This is needed as previously we stored raw time.Duration values which
-- are 64 bit integers and are used to express duration in nanoseconds however
-- the intent is to store invoice expiry in seconds.
UPDATE invoices SET expiry = 3600;
