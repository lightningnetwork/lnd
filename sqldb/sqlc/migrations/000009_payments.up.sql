-- ─────────────────────────────────────────────
-- Payment System Schema Migration
-- ─────────────────────────────────────────────
-- This migration creates the complete payment system schema including:
-- - Payment intents (BOLT 11/12 invoices, offers)
-- - Payment attempts and HTLC tracking
-- - Route hops and custom TLV records
-- - Resolution tracking for settled/failed payments
-- ─────────────────────────────────────────────

-- ─────────────────────────────────────────────
-- Payment Intents Table
-- ─────────────────────────────────────────────
-- Stores the descriptor of what the payment is paying for.
-- Depending on the type, the payload might contain:
-- - BOLT 11 invoice data
-- - BOLT 12 offer data  
-- - NULL for legacy hash-only/keysend style payments
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_intents (
    -- Primary key for the intent record
    id INTEGER PRIMARY KEY,

    -- The type of intent (e.g. 0 = bolt11_invoice, 1 = bolt12_offer)
    -- Uses SMALLINT (int16) for efficient storage of enum values
    intent_type SMALLINT NOT NULL,

    -- The serialized payload for the payment intent
    -- Content depends on type - could be invoice, offer, or NULL
    intent_payload BLOB
);

-- Index for efficient querying by intent type
CREATE INDEX IF NOT EXISTS idx_payment_intents_type
ON payment_intents(intent_type);

-- Unique constraint for deduplication of payment intents
CREATE UNIQUE INDEX IF NOT EXISTS idx_payment_intents_unique
ON payment_intents(intent_type, intent_payload);

-- ─────────────────────────────────────────────
-- Payments Table
-- ─────────────────────────────────────────────
-- Stores all payments including all known payment types:
-- - Legacy payments
-- - Multi-Path Payments (MPP)
-- - Atomic Multi-Path Payments (AMP)
-- - Blinded payments
-- - Keysend payments
-- - Spontaneous AMP payments
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payments (
    -- Primary key for the payment record
    id INTEGER PRIMARY KEY,

    -- Optional reference to the payment intent this payment was derived from
    -- Links to BOLT 11 invoice, BOLT 12 offer, etc.
    intent_id BIGINT REFERENCES payment_intents (id),

    -- The amount of the payment in millisatoshis
    amount_msat BIGINT NOT NULL,

    -- Timestamp when the payment was created
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- Logical identifier for the payment
    -- For legacy + MPP: matches the HTLC hash
    -- For AMP: the setID
    -- For future intent types: any unique payment-level key
    payment_identifier BLOB NOT NULL,
    
    -- The reason for payment failure (only set if payment has failed)
    -- Integer enum type indicating failure reason
    fail_reason INTEGER,

    -- Ensure payment identifiers are unique across all payments
    CONSTRAINT idx_payments_payment_identifier_unique 
    UNIQUE (payment_identifier)
);

-- Index for efficient querying by creation time (for chronological ordering)
CREATE INDEX IF NOT EXISTS idx_payments_created_at 
ON payments(created_at);

-- ─────────────────────────────────────────────
-- Payment HTLC Attempts Table
-- ─────────────────────────────────────────────
-- Stores all HTLC attempts for a payment. A payment can have multiple
-- HTLC attempts depending on whether the payment is split and also
-- if some attempts fail and need to be retried.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_htlc_attempts (
    -- Primary key for the HTLC attempt record
    id INTEGER PRIMARY KEY,

    -- The index of the HTLC attempt
    -- TODO: This will be removed and the primary key will be used only
    attempt_index BIGINT NOT NULL,
    
    -- Reference to the parent payment
    payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

    -- The session key of the HTLC attempt (also known as ephemeral key
    -- of the Sphinx packet used for onion routing)
    session_key BLOB NOT NULL,
    
    -- Timestamp when the HTLC attempt was created
    attempt_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

    -- The payment hash for the payment attempt
    -- The hash the HTLC will be locked to - this does not need to be
    -- equal to the payment level identifier (e.g., for AMP payments)
    payment_hash BLOB NOT NULL,

    -- First hop amount in millisatoshis of the HTLC attempt
    -- Normally the same as the total amount of the route, but when using
    -- custom channels this might be different
    first_hop_amount_msat BIGINT NOT NULL,

    -- ─────────────────────────────────────────────
    -- Route Information for the HTLC Attempt
    -- ─────────────────────────────────────────────
    -- Every attempt has one route, so there is a 1:1 relationship between
    -- attempts and routes. The route itself can be found in the hops table.
    -- ─────────────────────────────────────────────

    -- The total time lock of the route (in blocks)
    route_total_time_lock INTEGER NOT NULL,

    -- The total amount of the route in millisatoshis
    route_total_amount BIGINT NOT NULL,

    -- The source key of the route (our node's public key)
    route_source_key BLOB NOT NULL,

    -- Ensure attempt indices are unique across all attempts
    CONSTRAINT idx_htlc_attempt_index_unique 
    UNIQUE (attempt_index),

    -- Ensure session keys are unique (each attempt has unique session key)
    CONSTRAINT idx_htlc_session_key_unique 
    UNIQUE (session_key)
);

-- Index for efficient querying by payment ID (find all attempts for a payment)
CREATE INDEX IF NOT EXISTS idx_htlc_payment_id 
ON payment_htlc_attempts(payment_id);

-- Index for efficient querying by attempt index (for lookups and joins)
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_index 
ON payment_htlc_attempts(attempt_index);

-- Index for efficient querying by payment hash (for HTLC matching)
CREATE INDEX IF NOT EXISTS idx_htlc_payment_hash 
ON payment_htlc_attempts(payment_hash);

-- Index for efficient querying by attempt time (for chronological ordering)
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_time 
ON payment_htlc_attempts(attempt_time);

-- ─────────────────────────────────────────────
-- HTLC Attempt Resolutions Table
-- ─────────────────────────────────────────────
-- Stores resolution metadata for HTLC attempts. Rows appear once an
-- attempt settles or fails, providing the final outcome and timing.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_htlc_attempt_resolutions (
    -- Primary key referencing the HTLC attempt
    -- TODO: This will be removed and the primary key will be used only
    attempt_index INTEGER PRIMARY KEY 
    REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

    -- Timestamp when the attempt was resolved (settled or failed)
    resolution_time TIMESTAMP NOT NULL,

    -- Outcome of the attempt: 1 = settled, 2 = failed
    resolution_type INTEGER NOT NULL CHECK (resolution_type IN (1, 2)),

    -- Settlement payload (only populated for settled attempts)
    -- Contains the preimage that proves payment completion
    settle_preimage BLOB,

    -- Failure payload (only populated for failed attempts)
    -- Index of the node that sent the failure
    failure_source_index INTEGER,
    
    -- HTLC failure reason code
    htlc_fail_reason INTEGER,
    
    -- Failure message from the failing node, this message is binary encoded
    -- using the lightning wire protocol, see also lnwire/onion_error.go
    failure_msg BLOB,

    -- Ensure data integrity: settled attempts must have preimage,
    -- failed attempts must not have preimage
    CHECK (
        (resolution_type = 1 AND settle_preimage IS NOT NULL AND 
         failure_source_index IS NULL AND htlc_fail_reason IS NULL AND 
         failure_msg IS NULL)
        OR
        (resolution_type = 2 AND settle_preimage IS NULL)
    )
);

-- Index for efficient querying by resolution type (settled vs failed)
CREATE INDEX IF NOT EXISTS idx_htlc_resolutions_type 
ON payment_htlc_attempt_resolutions(resolution_type);

-- Index for efficient querying by resolution time (for chronological analysis)
CREATE INDEX IF NOT EXISTS idx_htlc_resolutions_time 
ON payment_htlc_attempt_resolutions(resolution_time);

-- ─────────────────────────────────────────────
-- Payment Route Hops Table
-- ─────────────────────────────────────────────
-- Stores the individual hops of a payment route. An attempt has only
-- one route, but a route can consist of several hops through the network.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_route_hops (
    -- Primary key for the hop record
    id INTEGER PRIMARY KEY,

    -- Reference to the HTLC attempt this hop belongs to
    htlc_attempt_index BIGINT NOT NULL 
    REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

    -- The order/index of this hop within the route (0-based)
    hop_index INTEGER NOT NULL,

    -- The public key of the hop (node's public key)
    pub_key BLOB,

    -- The short channel ID of the hop (channel identifier)
    scid TEXT NOT NULL,

    -- The outgoing time lock of the hop (in blocks)
    outgoing_time_lock INTEGER NOT NULL,

    -- The amount to forward to the next hop (in millisatoshis)
    amt_to_forward BIGINT NOT NULL,

    -- The metadata blob transmitted to the hop (onion payload)
    meta_data BLOB,

    -- Ensure each attempt can only have one hop at each hop index
    -- This prevents duplicate hops in the same position
    CONSTRAINT idx_route_hops_unique_hop_per_attempt 
    UNIQUE (htlc_attempt_index, hop_index)
);

-- Index for efficient querying by attempt index (find all hops for an attempt)
CREATE INDEX IF NOT EXISTS idx_route_hops_htlc_attempt_index 
ON payment_route_hops(htlc_attempt_index);

-- ─────────────────────────────────────────────
-- Per-Hop Payload Tables
-- ─────────────────────────────────────────────
-- These tables store specialized payload data for different payment types.
-- Each table is only populated for hops that require that specific payload.
-- ─────────────────────────────────────────────

-- ─────────────────────────────────────────────
-- MPP (Multi-Path Payment) Payload Table
-- ─────────────────────────────────────────────
-- Stores MPP-specific payload data. Only present for the final hop
-- of an MPP attempt, containing payment address and total amount info.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_route_hop_mpp (
    -- Primary key referencing the hop
    hop_id INTEGER PRIMARY KEY 
    REFERENCES payment_route_hops (id) ON DELETE CASCADE,

    -- The payment address of the MPP path (for payment correlation)
    payment_addr BLOB NOT NULL,

    -- The total amount of the MPP payment in millisatoshis
    -- This is the sum of all parts in the multi-path payment
    total_msat BIGINT NOT NULL
);

-- ─────────────────────────────────────────────
-- AMP (Atomic Multi-Path Payment) Payload Table
-- ─────────────────────────────────────────────
-- Stores AMP-specific payload data. Only present for the final hop
-- of an AMP attempt, containing share information for atomicity.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_route_hop_amp (
    -- Primary key referencing the hop
    hop_id INTEGER PRIMARY KEY 
    REFERENCES payment_route_hops (id) ON DELETE CASCADE,

    -- The root share of the AMP path (for share reconstruction)
    root_share BLOB NOT NULL,

    -- The set ID of the AMP path (groups related AMP parts)
    set_id BLOB NOT NULL,

    -- The child index of the AMP path (identifies this part)
    child_index INTEGER NOT NULL
);

-- ─────────────────────────────────────────────
-- Blinded Route Payload Table
-- ─────────────────────────────────────────────
-- Stores blinded route payload data. Rows only exist for hops that
-- are part of a blinded path, providing privacy-preserving routing.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_route_hop_blinded (
    -- Primary key referencing the hop
    hop_id INTEGER PRIMARY KEY 
    REFERENCES payment_route_hops (id) ON DELETE CASCADE,

    -- The encrypted payload for the blinded hop
    encrypted_data BLOB NOT NULL,

    -- Only set for the introduction point of the blinded path
    -- Contains the blinding point for the introduction node
    blinding_point BLOB,

    -- Only set for the final hop in the blinded path
    -- Contains the total amount for the entire blinded path
    blinded_path_total_amt BIGINT
);

-- ─────────────────────────────────────────────
-- Custom TLV Records Tables
-- ─────────────────────────────────────────────
-- These tables store custom TLV (Type-Length-Value) records associated
-- with payments, attempts, and hops. This is a denormalized structure
-- designed to simplify cascade deletions, as each record is owned by
-- a single parent entity.
-- ─────────────────────────────────────────────

-- ─────────────────────────────────────────────
-- Payment-Level First Hop Custom Records
-- ─────────────────────────────────────────────
-- Stores custom TLV records that are part of the first hop of a payment.
-- These records are sent to the first hop and are payment-level data.
-- 
-- NOTE: This relates to the custom tlv record data which is sent to the first
-- hop in the wire message (UpdateAddHTLC) NOT the onion packet.
-- 
-- TODO(ziggie): We store mostly redundant data here and on the attempt level.
-- This might be improved in the future to reduce duplication.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_first_hop_custom_records (
    -- Primary key for the custom record
    id INTEGER PRIMARY KEY,
    
    -- Reference to the parent payment
    payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,
    
    -- The TLV type identifier (must be >= 65536 for custom records)
    key BIGINT NOT NULL,
    
    -- The TLV value data
    value BLOB NOT NULL,
    
    -- Ensure we only store custom TLV records (not standard ones)
    CHECK (key >= 65536),
    
    -- Ensure each payment can only have one record per TLV type
    CONSTRAINT idx_payment_first_hop_custom_records_unique 
    UNIQUE (payment_id, key)
);

-- ─────────────────────────────────────────────
-- Attempt-Level First Hop Custom Records
-- ─────────────────────────────────────────────
-- Stores custom TLV records for the first hop on the route level.
-- These might be different from the payment-level first hop records
-- in case of custom channels or route-specific modifications.
-- 
-- NOTE: This relates to the custom tlv record data which is sent to the first
-- hop in the wire message (UpdateAddHTLC) NOT the onion packet.
-- 
-- TODO(ziggie): We store mostly redundant data here and on the payment level.
-- This might be improved in the future to reduce duplication.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_attempt_first_hop_custom_records (
    -- Primary key for the custom record
    id INTEGER PRIMARY KEY,
    
    -- Reference to the parent HTLC attempt
    htlc_attempt_index BIGINT NOT NULL 
    REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,
    
    -- The TLV type identifier (must be >= 65536 for custom records)
    key BIGINT NOT NULL,
    
    -- The TLV value data
    value BLOB NOT NULL,
    
    -- Ensure we only store custom TLV records (not standard ones)
    CHECK (key >= 65536),
    
    -- Ensure each attempt can only have one record per TLV type
    CONSTRAINT idx_payment_attempt_first_hop_custom_records_unique 
    UNIQUE (htlc_attempt_index, key)
);

-- ─────────────────────────────────────────────
-- Hop-Level Custom Records
-- ─────────────────────────────────────────────
-- Stores custom TLV records associated with a specific hop within
-- a payment route. These records are sent to that specific hop
-- and are hop-specific data.
-- ─────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS payment_hop_custom_records (
    -- Primary key for the custom record
    id INTEGER PRIMARY KEY,
    
    -- Reference to the parent hop
    hop_id BIGINT NOT NULL REFERENCES payment_route_hops (id) ON DELETE CASCADE,
    
    -- The TLV type identifier (must be >= 65536 for custom records)
    key BIGINT NOT NULL,
    
    -- The TLV value data
    value BLOB NOT NULL,
    
    -- Ensure we only store custom TLV records (not standard ones)
    CHECK (key >= 65536),
    
    -- Ensure each hop can only have one record per TLV type
    CONSTRAINT idx_payment_hop_custom_records_unique 
    UNIQUE (hop_id, key)
);