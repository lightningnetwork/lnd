-- Partial index for startup payment recovery queries that filter on
-- fail_reason IS NULL and walk payment IDs in ascending order.
CREATE INDEX IF NOT EXISTS idx_payments_no_fail_reason
ON payments(id) WHERE fail_reason IS NULL;
