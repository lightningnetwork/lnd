-- ─────────────────────────────────────────────
-- Liquidity Intervals
-- ─────────────────────────────────────────────
-- Stores what the interval router believes about the liquidity available in
-- one direction of one channel. Unlike mission control, which records a
-- penalty per node pair that fades with time, this is an amount interval:
-- bounded below by the largest amount the router has watched pass and above by
-- the smallest it has watched fail.
--
-- A row is written per direction, so an ordinary channel occupies two rows
-- whose scid agrees and whose node columns are swapped. A scid of eight zero
-- bytes is reserved for a belief held about a node pair as a whole rather than
-- about a specific channel, which is what the router falls back to when a pair
-- has several channels between it and an observation cannot say which one
-- carried the payment.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS liquidity_intervals (
    -- The short channel id of the channel, as eight big endian bytes. All
    -- zeroes means the row describes the node pair rather than one channel.
    scid BLOB NOT NULL,

    -- The public key of the node the liquidity flows away from.
    from_node BLOB NOT NULL,

    -- The public key of the node the liquidity flows towards.
    to_node BLOB NOT NULL,

    -- The largest amount in millisatoshis this direction has been proven to
    -- carry. Zero means nothing has been proven.
    lower_ok_msat BIGINT NOT NULL,

    -- The smallest amount in millisatoshis this direction has been proven not
    -- to carry. Zero means no failure has been observed.
    upper_fail_msat BIGINT NOT NULL,

    -- The best guess in millisatoshis at the balance available.
    estimate_msat BIGINT NOT NULL,

    -- How much evidence stands behind the estimate, in parts per million of a
    -- confidence that ranges from zero to one. Stored as an integer because
    -- the schema has no floating point column anywhere else.
    confidence_ppm BIGINT NOT NULL,

    -- How many observations of each kind have landed on this direction.
    successes BIGINT NOT NULL,
    failures BIGINT NOT NULL,

    -- Which side of the bimodal liquidity distribution this direction appears
    -- to sit on: -1 depleted, 0 unclassified, 1 saturated.
    liquidity_mode INTEGER NOT NULL,

    -- When this belief was last written. Used only to decide which rows to
    -- drop when the table outgrows its bound; the model itself has no clock
    -- and never expires a belief because time has passed.
    updated_at TIMESTAMP NOT NULL,

    PRIMARY KEY (scid, from_node, to_node)
);

-- Index supporting the read of the most recently written beliefs at startup
-- and the pruning of the oldest ones.
CREATE INDEX IF NOT EXISTS idx_liquidity_intervals_updated_at
ON liquidity_intervals(updated_at);
