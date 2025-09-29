-- ─────────────────────────────────────────────
-- Drop custom TLV record tables first (they have no dependents).
-- ─────────────────────────────────────────────

DROP TABLE IF EXISTS payment_hop_custom_records;
DROP TABLE IF EXISTS payment_attempt_first_hop_custom_records;
DROP TABLE IF EXISTS payment_first_hop_custom_records;

-- ─────────────────────────────────────────────
-- Drop per-hop payload tables before dropping the base hops table.
-- ─────────────────────────────────────────────

DROP TABLE IF EXISTS payment_route_hop_blinded;
DROP TABLE IF EXISTS payment_route_hop_amp;
DROP TABLE IF EXISTS payment_route_hop_mpp;

-- ─────────────────────────────────────────────
-- Drop route hops table and its indexes.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_route_hops_htlc_attempt_index;
DROP TABLE IF EXISTS payment_route_hops;

-- ─────────────────────────────────────────────
-- Drop HTLC attempt resolution table and its indexes.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_htlc_resolutions_type;
DROP INDEX IF EXISTS idx_htlc_resolutions_time;
DROP TABLE IF EXISTS payment_htlc_attempt_resolutions;

-- ─────────────────────────────────────────────
-- Drop HTLC attempts table and its indexes.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_htlc_payment_id;
DROP INDEX IF EXISTS idx_htlc_attempt_index;
DROP INDEX IF EXISTS idx_htlc_payment_hash;
DROP INDEX IF EXISTS idx_htlc_attempt_time;
DROP TABLE IF EXISTS payment_htlc_attempts;

-- ─────────────────────────────────────────────
-- Drop payments table and its indexes.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_payments_created_at;
DROP TABLE IF EXISTS payments;

-- ─────────────────────────────────────────────
-- Drop payment intents table and its indexes.
-- ─────────────────────────────────────────────

DROP INDEX IF EXISTS idx_payment_intents_type;
DROP TABLE IF EXISTS payment_intents;