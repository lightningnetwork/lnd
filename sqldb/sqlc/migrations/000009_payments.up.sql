-- payments table stores all payments including all known payment types (e.g.
-- legacy, mpp, amp, blinded, keysend, spontaneous_amp)
CREATE TABLE IF NOT EXISTS payments (
   -- The id of the payment, this identifies every payment.
  id INTEGER PRIMARY KEY,

  -- The payment request for the payment. Can be null if just pay to a hash
  -- or for keysends or spontaneous AMP-payments.
  payment_request BLOB,

  -- The amount of the payment in millisatoshis.
  amount_msat BIGINT NOT NULL,

  -- The timestamp of when the payment was created.
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The hash of the payment - UNIQUE constraint ensures no duplicates.
  payment_hash BLOB NOT NULL UNIQUE,

  -- The reason for the payment failure.
  -- Integer Enum Type. Only set if the payment has failed.
  fail_reason INTEGER
);

-- Index for the payments table to query by creation time.
CREATE INDEX IF NOT EXISTS idx_payments_created_at ON payments(created_at);

-- Index for the payments table to query by payment hash.
CREATE UNIQUE INDEX IF NOT EXISTS idx_payments_payment_hash ON payments(payment_hash);


-- payment_htlc_attempts table stores all htlc attempts for a payment. Every
-- payment can have multiple htlc attempts depending on wether the payment is
-- splitted and also if some attempts fail.
CREATE TABLE IF NOT EXISTS payment_htlc_attempts (
  -- The id of the htlc attempt.
  id INTEGER PRIMARY KEY,

  -- The index of the htlc attempt. Needs to be separate for now but we should
  -- eventually just use the primary key of the attempt table.
  attempt_index BIGINT NOT NULL UNIQUE,

  -- The id of the payment.
  payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

  -- The session key of the htlc attempt (also know as ephemeral key of the
  -- Sphinx packet).
  session_key BLOB NOT NULL UNIQUE,

  -- The timestamp of when the htlc attempt was created.
  attempt_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The payment hash for the payment attempt.
  payment_hash BLOB NOT NULL,

  -- First hop amount in millisatoshis of the htlc attempt. This is normally
  -- the same as the total amount of the route but when using custom channels
  -- this might be different.
  first_hop_amount_msat BIGINT NOT NULL,

  -- ─────────────────────────────────────────────
  -- The following is the route info for the htlc attempt.
  -- We do not normalize it for performance reasons.
  -- ─────────────────────────────────────────────

  -- The total time lock of the route.
  route_total_time_lock INTEGER NOT NULL,

  -- The total amount of the route.
  route_total_amount BIGINT NOT NULL,

  -- The source key of the route.
  route_source_key BLOB NOT NULL,

  -- ─────────────────────────────────────────────
  -- The following is the fail info for the htlc attempt.
  -- We do not normalize it for performance reasons.
  -- ─────────────────────────────────────────────

  -- FailInfo only set if the attempt has failed.
  -- The index of the failure source.
  failure_source_index INTEGER,

  -- The reason for the htlc fail.
  -- Integer Enum Type.
  htlc_fail_reason INTEGER,

  -- The failure message.
  failure_msg BLOB,

   -- The timestamp of when the htlc attempt failed.
  fail_time TIMESTAMP,

  -- ─────────────────────────────────────────────
  -- The following is the settle info for the htlc attempt.
  -- We do not normalize it for performance reasons.
  -- ─────────────────────────────────────────────

  -- The preimage of the htlc settle info.
  -- Many attempts can have the same preimage.
  settle_preimage BLOB,

  -- The timestamp of when the htlc settle info was created.
  settle_time TIMESTAMP,

  -- Constraint: If one time is set, the other must be NULL (mutual exclusivity)
  -- Both can be NULL for pending HTLCs. This makes sure the database is 
  -- consistent.
  -- 
  CONSTRAINT check_htlc_time_mutual_exclusive CHECK (
    (fail_time IS NULL) OR (settle_time IS NULL)
  )
);

-- Indexes for the htlc attempts to query by status.
CREATE INDEX IF NOT EXISTS idx_htlc_pending ON payment_htlc_attempts(payment_id) 
WHERE fail_time IS NULL AND settle_time IS NULL;

CREATE INDEX IF NOT EXISTS idx_htlc_failed ON payment_htlc_attempts(payment_id) 
WHERE fail_time IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_htlc_settled ON payment_htlc_attempts(payment_id) 
WHERE settle_time IS NOT NULL;

-- Index for the htlc attempts to query by payment id.
CREATE INDEX IF NOT EXISTS idx_htlc_payment_id ON payment_htlc_attempts(payment_id);

-- Index for the htlc attempts to query by attempt index.
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_index ON payment_htlc_attempts(attempt_index);

-- Index for the htlc attempts to query by payment hash.
CREATE INDEX IF NOT EXISTS idx_htlc_payment_hash ON payment_htlc_attempts(payment_hash);

-- Index for the htlc attempts by attempt time.
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_time ON payment_htlc_attempts(attempt_time);


-- payment_route_hops table stores the hops of a payment route. And attempt
-- has only one route but a route can consist of several hops.
CREATE TABLE IF NOT EXISTS payment_route_hops (
  -- The id of the hop.
  id INTEGER PRIMARY KEY,

  -- The index of the htlc attempt this hop belongs to.
  htlc_attempt_index BIGINT NOT NULL REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

  -- The order/index of this hop within the route.
  hop_index INTEGER NOT NULL,

  -- The public key of the hop.
  pub_key BLOB,

  -- The short channel id of the hop.
  scid TEXT NOT NULL,

  -- The outgoing time lock of the hop.
  outgoing_time_lock INTEGER NOT NULL,

  -- The amount to forward of the hop.
  amt_to_forward BIGINT NOT NULL,

  -- The meta data of the hop.
  meta_data BLOB,

  -- legacy_payload if true, then this signals that this node doesn't
  -- understand the new TLV payload, so we must instead use the legacy
  -- payload.
  --
  -- NOTE: we should no longer ever create a Hop with Legacy set to true.
  -- The only reason we are keeping this member is that it could be the
  -- case that we have serialised hops persisted to disk where
  -- LegacyPayload is true when migrating from the old payment db.
  legacy_payload BOOLEAN NOT NULL DEFAULT FALSE,

  -- ─────────────────────────────────────────────
  -- MPP specific fields (only set for last hop with MPP which should be the
  -- standard case). Payments not setting these fields are legacy payments and
  -- should not be created anymore. But they exists in case of old payments
  -- made.
  -- ─────────────────────────────────────────────

  -- The payment address of the MPP path
  mpp_payment_addr BLOB,

  -- The total amount of the MPP payment in millisatoshis.
  mpp_total_msat BIGINT,

  -- ─────────────────────────────────────────────
  -- AMP specific fields (only set for last hop with AMP)
  -- ─────────────────────────────────────────────

  -- The root share of the AMP path
  amp_root_share BLOB,

  -- The set id of the AMP path
  amp_set_id BLOB,

  -- The child index of the AMP path
  amp_child_index INTEGER,

  -- ─────────────────────────────────────────────
  -- Blinded path specific fields (only set for hops which are part of the
  -- blinded path)
  -- ─────────────────────────────────────────────

  -- The encrypted data of the blinded hop
  encrypted_data BLOB,

  -- Only set for the introduction point for the other hops in the path the
  -- blinded point is part of the encrypted data.
  -- 
  -- Check with Elle whether this can only be part of the introduction node.
  blinding_point BLOB,

  -- The total amount of the blinded path only set for the last hop in the
  -- blinded path.
  blinded_path_total_amt BIGINT,

  -- Ensure unique hop per attempt
  UNIQUE(htlc_attempt_index, hop_index)
);

-- Index for the route hops to query by attempt index.
CREATE INDEX IF NOT EXISTS idx_route_hops_htlc_attempt_index ON payment_route_hops(htlc_attempt_index);


-- first_hop_custom_records table only set to the first hop of the payment.
CREATE TABLE IF NOT EXISTS payment_first_hop_custom_records (
  -- The id of the first hop custom record.
  id INTEGER PRIMARY KEY,

  -- The key of the tlv record.
  key BIGINT NOT NULL,

  -- The value of the tlv record.
  value BLOB NOT NULL,

  -- The id of the payment this first hop custom record belongs to.
  payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

  -- Ensure each payment can only have one record per key.
  UNIQUE(payment_id, key)
);

-- Index of the payment id.
CREATE INDEX IF NOT EXISTS idx_first_hop_custom_records_payment_id ON payment_first_hop_custom_records(payment_id);

-- route_custom_records table
CREATE TABLE IF NOT EXISTS payment_htlc_attempt_custom_records (
  -- The id of the custom record.
  id INTEGER PRIMARY KEY,
  
  -- The key of the tlv record.
  key BIGINT NOT NULL,

  -- The value of the tlv record.
  value BLOB NOT NULL,

  -- The id of the htlc attempt.
  htlc_attempt_index BIGINT NOT NULL REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

  -- Ensure each htlc attempt can only have one record per key.
  UNIQUE(htlc_attempt_index, key)
);

-- Index of the htlc attempt id.
CREATE INDEX IF NOT EXISTS idx_htlc_attempt_custom_records_htlc_attempt_index ON payment_htlc_attempt_custom_records(htlc_attempt_index);


-- payment_route_hop_custom_records table
CREATE TABLE IF NOT EXISTS payment_route_hop_custom_records (
  -- The id of the custom record.
  id INTEGER PRIMARY KEY,

  -- The key of the tlv record.
  key BIGINT NOT NULL,

  -- The value of the tlv record.
  value BLOB NOT NULL,

  -- The id of the hop.
  hop_id BIGINT NOT NULL REFERENCES payment_route_hops (id) ON DELETE CASCADE,

  -- Ensure each hop can only have one record per key.
  UNIQUE(hop_id, key)
);

-- Index of the hop id.
CREATE INDEX IF NOT EXISTS idx_route_hop_custom_records_hop_id ON payment_route_hop_custom_records(hop_id);
