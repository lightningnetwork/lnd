-- Add the hold_times column to payment_htlc_attempt_resolutions to record
-- per-hop hold times reported via attribution data on failed HTLCs. The
-- value is a length-prefixed sequence of big-endian uint32 entries; nil or
-- empty arrays are stored as NULL.
ALTER TABLE payment_htlc_attempt_resolutions ADD COLUMN hold_times BLOB;
