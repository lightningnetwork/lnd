-- ─────────────────────────────────────────────
-- Payment Duplicate Records Table
-- ─────────────────────────────────────────────
-- Stores duplicate payment records that were created in older versions
-- of lnd. This table is intentionally minimal and is expected to be
-- temporary (dropped after KV migrations complete).
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_duplicates (
    -- Primary key for the duplicate record
    id INTEGER PRIMARY KEY,

    -- Reference to the primary payment this duplicate belongs to
    payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

    -- Logical identifier for the duplicate payment
    payment_identifier BLOB NOT NULL,

    -- Amount of the duplicate payment in millisatoshis
    amount_msat BIGINT NOT NULL,

    -- Timestamp when the duplicate payment was created
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- Failure reason for failed payments (if known)
    fail_reason INTEGER,

    -- Settlement payload for succeeded payments (if known)
    settle_preimage BLOB,

    -- Settlement time for succeeded payments (if known)
    settle_time TIMESTAMP,

    -- Ensure we record either a failure reason or settlement data
    CONSTRAINT chk_payment_duplicates_outcome
    CHECK (fail_reason IS NOT NULL OR settle_preimage IS NOT NULL)
);

-- Index for efficient lookup by primary payment
CREATE INDEX IF NOT EXISTS idx_payment_duplicates_payment_id
ON payment_duplicates(payment_id);
