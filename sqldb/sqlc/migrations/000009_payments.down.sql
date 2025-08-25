-- ─────────────────────────────────────────────
-- Delete payments related data.
-- ─────────────────────────────────────────────

-- Delete indexes for the payments table.
DROP INDEX IF EXISTS idx_payments_created_at;
DROP INDEX IF EXISTS idx_payments_payment_hash;

-- Delete the payments table.
DROP TABLE IF EXISTS payments;

-- ─────────────────────────────────────────────
-- Delete htlc attempt related data.
-- ─────────────────────────────────────────────

-- Delete attempt indexes and the table itself.
DROP INDEX IF EXISTS idx_htlc_pending;
DROP INDEX IF EXISTS idx_htlc_failed;
DROP INDEX IF EXISTS idx_htlc_settled;
DROP INDEX IF EXISTS idx_htlc_payment_id;
DROP INDEX IF EXISTS idx_htlc_attempt_index;
DROP INDEX IF EXISTS idx_htlc_payment_hash;
DROP INDEX IF EXISTS idx_htlc_attempt_time;

-- Delete the htlc attempts table.
DROP TABLE IF EXISTS payment_htlc_attempts;

-- ─────────────────────────────────────────────
-- Delete htlc attempt custom records related data.
-- ─────────────────────────────────────────────

-- Delete indexes for the htlc attempt custom records table.
DROP INDEX IF EXISTS idx_htlc_attempt_custom_records_htlc_attempt_index;

-- Delete the htlc attempt custom records table.
DROP TABLE IF EXISTS payment_htlc_attempt_custom_records;


-- ─────────────────────────────────────────────
-- Delete route hop related data.
-- ─────────────────────────────────────────────

-- Delete indexes for the route hops table.
DROP INDEX IF EXISTS idx_route_hops_htlc_attempt_index;

-- Delete the route hops table.
DROP TABLE IF EXISTS payment_route_hops;

-- ─────────────────────────────────────────────
-- Delete first hop custom records related data.
-- ─────────────────────────────────────────────

-- Delete indexes for the first hop custom records table.
DROP INDEX IF EXISTS idx_first_hop_custom_records_payment_id;

-- Delete the first hop custom records table.
DROP TABLE IF EXISTS payment_first_hop_custom_records;

-- ─────────────────────────────────────────────
-- Delete route hop custom records related data.
-- ─────────────────────────────────────────────

-- Delete indexes for the route hop custom records table.
DROP INDEX IF EXISTS idx_route_hop_custom_records_hop_id;

-- Delete the route hop custom records table.
DROP TABLE IF EXISTS payment_route_hop_custom_records;

