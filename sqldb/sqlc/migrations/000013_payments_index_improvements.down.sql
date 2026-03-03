-- Restore the composite indexes removed in the up migration.
DROP INDEX IF EXISTS idx_htlc_payment_id_attempt_time;
DROP INDEX IF EXISTS idx_htlc_resolutions_type_attempt_index;

-- Restore the redundant indexes that were dropped in the up migration.
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_index
ON payment_htlc_attempts(attempt_index);

CREATE INDEX IF NOT EXISTS idx_route_hops_htlc_attempt_index
ON payment_route_hops(htlc_attempt_index);
