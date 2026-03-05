-- ─────────────────────────────────────────────
-- Remove redundant indexes from the payments schema.
-- ─────────────────────────────────────────────
-- Drop two explicit indexes that duplicate indexes already created by UNIQUE
-- constraints:
--
--  - payment_htlc_attempts(attempt_index) duplicates UNIQUE(attempt_index)
--  - payment_route_hops(htlc_attempt_index) duplicates
--    UNIQUE(htlc_attempt_index, hop_index)
--
-- This reduces write/index maintenance overhead without changing query
-- capabilities, since the UNIQUE-backed autoindexes already satisfy these
-- lookups via exact and leftmost-prefix matching.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_htlc_attempt_index;
DROP INDEX IF EXISTS idx_route_hops_htlc_attempt_index;

-- ─────────────────────────────────────────────
-- Add composite indexes for hot payment query paths.
-- ─────────────────────────────────────────────
-- Add two composite indexes to better match high-frequency query patterns
-- observed in the payment lifecycle.
-- ─────────────────────────────────────────────

-- Composite index for batched attempt fetches that filter by payment_id and
-- order by attempt_time. This matches FetchHtlcAttemptsForPayments:
-- WHERE payment_id IN (...) ORDER BY payment_id, attempt_time.
CREATE INDEX IF NOT EXISTS idx_htlc_payment_id_attempt_time
ON payment_htlc_attempts(payment_id, attempt_time);

-- Composite index for delete paths that first filter failed resolutions by
-- resolution_type and then join/delete by attempt_index. This matches
-- DeleteFailedAttempts.
CREATE INDEX IF NOT EXISTS idx_htlc_resolutions_type_attempt_index
ON payment_htlc_attempt_resolutions(resolution_type, attempt_index);
