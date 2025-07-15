-- Drop all tables created in the payments migration
DROP TABLE IF EXISTS payment_htlc_attempts;
DROP TABLE IF EXISTS payment_route_hops;
DROP TABLE IF EXISTS payment_first_hop_custom_records;
DROP TABLE IF EXISTS payment_route_hop_custom_records;
DROP TABLE IF EXISTS payments;
DROP TABLE IF EXISTS duplicate_payment_htlc_attempts;
DROP TABLE IF EXISTS duplicate_payment_route_hops;
DROP TABLE IF EXISTS duplicate_payment_route_hop_custom_records;
DROP TABLE IF EXISTS duplicate_payments;
DROP TABLE IF EXISTS payment_sequences;

DROP INDEX IF EXISTS idx_htlc_pending;
DROP INDEX IF EXISTS idx_htlc_failed;
DROP INDEX IF EXISTS idx_htlc_settled;
DROP INDEX IF EXISTS idx_route_hops_attempt;
DROP INDEX IF EXISTS idx_route_hops_chan_id;
DROP INDEX IF EXISTS idx_first_hop_records_payment;
DROP INDEX IF EXISTS idx_hop_records_hop;
DROP INDEX IF EXISTS idx_duplicate_htlc_pending;
DROP INDEX IF EXISTS idx_duplicate_htlc_failed;
DROP INDEX IF EXISTS idx_duplicate_htlc_settled;