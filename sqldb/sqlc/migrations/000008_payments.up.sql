-- Create a table to track the next HTLC attempt ID.
CREATE TABLE IF NOT EXISTS payment_sequences (
    name TEXT PRIMARY KEY,
    current_value BIGINT NOT NULL
);

-- Initialize the sequences.
INSERT INTO payment_sequences (name, current_value) VALUES 
('htlc_attempt_index', 0) ON CONFLICT (name) DO NOTHING;

--- payments table
CREATE TABLE IF NOT EXISTS payments (
   -- The id of the payment, this identifies every payment.
  id INTEGER PRIMARY KEY,

  -- The type of the payment (currently not used)
  -- Proposed values:
  -- 0: legacy
  -- 1: mpp
  -- 2: amp
  -- 3: blinded
  -- 4: keysend
  -- 5: spontaneous_amp
  payment_type INTEGER,

  -- The payment request for the payment. Can be null if just pay to a hash
  -- or for keysends/spontaneous AMP-payments.
  payment_request BLOB,

  -- The amount of the payment in millisatoshis.
  amount_msat BIGINT NOT NULL,

  -- The timestamp of when the payment was created.
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The hash of the payment - UNIQUE constraint ensures no duplicates
  payment_hash BLOB NOT NULL UNIQUE,

  -- The reason for the payment failure.
  -- Integer Enum Type.
  -- only set if the payment has failed.
  fail_reason INTEGER,

  -- has_duplicate_payment is set to true if the payment has duplicate payments
  -- with the same payment hash which are legacy payments (only payments prior
  -- to MPP payments ? Can we instead use the payment type here?). They are
  -- tracked in a separate table.
  has_duplicate_payment BOOLEAN NOT NULL DEFAULT FALSE
);

--- payment_htlc_attempts table
CREATE TABLE IF NOT EXISTS payment_htlc_attempts (
  -- The id of the htlc attempt.
  id INTEGER PRIMARY KEY,

  -- The attempt index of the htlc attempt which needs to alway be incrementing
  -- because this ID is also use in the htlc switch to identify the attempt so
  -- we cannot reuse the id for the attempt index.
  attempt_index BIGINT NOT NULL UNIQUE,

  -- The id of the payment.
  payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

  -- The session key of the htlc attempt.
  session_key BLOB NOT NULL UNIQUE,

  -- The timestamp of when the htlc attempt was created.
  attempt_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The payment hash for the payment attempt.
  payment_hash BLOB NOT NULL,

  -- The following is the route info for the htlc attempt.

  -- The total time lock of the route.
  route_total_timelock INTEGER NOT NULL,

  -- The total amount of the route.
  route_total_amount BIGINT NOT NULL,

  -- The first hop amount of the route.
  route_first_hop_amount BIGINT,

  -- The source key of the route.
  route_source_key BLOB NOT NULL,

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

  -- SettleInfo only set if the attempt has settled.

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
CREATE INDEX idx_htlc_pending ON payment_htlc_attempts(payment_id) 
WHERE fail_time IS NULL AND settle_time IS NULL;

CREATE INDEX idx_htlc_failed ON payment_htlc_attempts(payment_id) 
WHERE fail_time IS NOT NULL;

CREATE INDEX idx_htlc_settled ON payment_htlc_attempts(payment_id) 
WHERE settle_time IS NOT NULL;

--- payment_route_hops table
CREATE TABLE IF NOT EXISTS payment_route_hops (
  -- The id of the hop.
  id INTEGER PRIMARY KEY,

  -- The attempt index this hop belongs to.
  htlc_attempt_index BIGINT NOT NULL REFERENCES payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

  -- The order/index of this hop within the route.
  hop_index INTEGER NOT NULL,

  -- The public key of the hop.
  pub_key BLOB,

  -- The channel id of the hop.
  chan_id TEXT NOT NULL,

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
  -- LegacyPayload is true.
  legacy_payload BOOLEAN NOT NULL DEFAULT FALSE,

  -- MPP specific fields (only set for last hop with MPP which should be the
  -- standard case). Payments not setting these fields are legacy payments and
  -- should not be created anymore. But they exists in case of old payments
  -- made.

  -- The payment address of the MPP path
  mpp_payment_addr BLOB,

  -- The total amount of the MPP payment in millisatoshis.
  mpp_total_msat BIGINT,

  -- AMP specific fields (only set for last hop with AMP)

  -- The root share of the AMP path
  amp_root_share BLOB,

  -- The set id of the AMP path
  amp_set_id BLOB,

  -- The child index of the AMP path
  amp_child_index INTEGER,

  -- Blinded path specific fields (only set for hops which are part of the
  -- blinded path)

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

--- first_hop_custom_records table only set to the first hop of the payment.
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

--- payment_route_hop_custom_records table
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

-- Performance indexes for common query patterns
CREATE INDEX IF NOT EXISTS idx_payments_created_at ON payments(created_at);
CREATE INDEX IF NOT EXISTS idx_payments_type ON payments(payment_type);

-- Index for payment attempts lookup
CREATE INDEX IF NOT EXISTS idx_htlc_attempts_payment_id ON payment_htlc_attempts(payment_id);
CREATE INDEX IF NOT EXISTS idx_htlc_attempts_payment_hash ON payment_htlc_attempts(payment_hash);
CREATE INDEX IF NOT EXISTS idx_htlc_attempts_time ON payment_htlc_attempts(attempt_time);
CREATE INDEX IF NOT EXISTS idx_htlc_attempts_status ON payment_htlc_attempts(payment_id, htlc_fail_reason, settle_preimage);

-- Index for route hops lookup
CREATE INDEX IF NOT EXISTS idx_route_hops_attempt ON payment_route_hops(htlc_attempt_index);
CREATE INDEX IF NOT EXISTS idx_route_hops_chan_id ON payment_route_hops(chan_id);

-- Index for custom records
CREATE INDEX IF NOT EXISTS idx_first_hop_records_payment ON payment_first_hop_custom_records(payment_id);
CREATE INDEX IF NOT EXISTS idx_hop_records_hop ON payment_route_hop_custom_records(hop_id);


-- The following tables are used to track duplicate payments and their htlc
-- attempts they are separate because new nodes will not have duplicate payments
-- and therefore we can avoid the overhead of tracking them in the main payments
-- table. Moreover we can figure out after the migration of the kvstore whether
-- duplicate payments exist and if not we can all the tables in a follow up
-- migration.

-- duplicate_payments table tracks duplicate payments with the same payment 
-- hash which are a relic of legacy payments and therefore this table should
-- only have entries and migrating old payment data.
CREATE TABLE IF NOT EXISTS duplicate_payments (
  -- The id of the duplicate payment.
  id INTEGER PRIMARY KEY,

  -- The id of the payment that is a duplicate of the duplicate payment.
  payment_id BIGINT NOT NULL REFERENCES payments (id) ON DELETE CASCADE,

  -- The payment hash of the duplicate payment. This references the same
  -- payment hash as the main payments table but allows multiple entries.
  payment_hash BLOB NOT NULL,

  -- The payment request for the payment. Can be null if just pay to a hash
  -- or for keysends/spontaneous AMP-payments.
  payment_request BLOB,

  -- The amount of the payment in millisatoshis.
  amount_msat BIGINT NOT NULL,

  -- The timestamp of when the payment was created.
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The reason for the payment failure.
  -- Integer Enum Type.
  -- only set if the payment has failed.
  fail_reason INTEGER
);


-- duplicate_payment_htlc_attempts table (for duplicate payments only)
CREATE TABLE IF NOT EXISTS duplicate_payment_htlc_attempts (
  -- The id of the htlc attempt.
  id INTEGER PRIMARY KEY,

  -- The attempt index of the htlc attempt which needs to alway be incrementing
  -- because this ID is also use in the htlc switch to identify the attempt so
  -- we cannot reuse the id for the attempt index.
  attempt_index BIGINT NOT NULL UNIQUE,

  -- The id of the duplicate payment.
  duplicate_payment_id BIGINT NOT NULL REFERENCES duplicate_payments (id) ON DELETE CASCADE,

  -- The session key of the htlc attempt.
  session_key BLOB NOT NULL UNIQUE,

  -- The timestamp of when the htlc attempt was created.
  attempt_time TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,

  -- The payment hash for the payment attempt.
  payment_hash BLOB NOT NULL,

  -- The following is the route info for the htlc attempt.

  -- The total time lock of the route.
  route_total_timelock INTEGER NOT NULL,

  -- The total amount of the route.
  route_total_amount BIGINT NOT NULL,

  -- The source key of the route.
  route_source_key BLOB NOT NULL,

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

  -- SettleInfo only set if the attempt has settled.

  -- The preimage of the htlc settle info.
  -- Many attempts can have the same preimage.
  settle_preimage BLOB,

  -- The timestamp of when the htlc settle info was created.
  settle_time TIMESTAMP,

  -- Constraint: If one time is set, the other must be NULL (mutual exclusivity)
  -- Both can be NULL for pending HTLCs. This makes sure the database is 
  -- consistent.
  -- 
  CONSTRAINT check_duplicate_htlc_time_mutual_exclusive CHECK (
    (fail_time IS NULL) OR (settle_time IS NULL)
  )
);

-- Index for duplicate payment attempts lookup
CREATE INDEX IF NOT EXISTS idx_duplicate_htlc_attempts_duplicate_payment_id ON duplicate_payment_htlc_attempts(duplicate_payment_id);
CREATE INDEX IF NOT EXISTS idx_duplicate_htlc_attempts_payment_hash ON duplicate_payment_htlc_attempts(payment_hash);
CREATE INDEX IF NOT EXISTS idx_duplicate_htlc_attempts_time ON duplicate_payment_htlc_attempts(attempt_time);
CREATE INDEX IF NOT EXISTS idx_duplicate_htlc_attempts_status ON duplicate_payment_htlc_attempts(duplicate_payment_id, htlc_fail_reason, settle_preimage);

--- duplicate_payment_route_hops table. Duplicate payments are always legacy
-- payments and therefore they always use the legacy payload.
CREATE TABLE IF NOT EXISTS duplicate_payment_route_hops (
  -- The id of the hop.
  id INTEGER PRIMARY KEY,

  -- The attempt index this hop belongs to.
  htlc_attempt_index BIGINT NOT NULL REFERENCES duplicate_payment_htlc_attempts (attempt_index) ON DELETE CASCADE,

  -- The order/index of this hop within the route.
  hop_index INTEGER NOT NULL,

  -- The public key of the hop.
  pub_key BLOB,

  -- The channel id of the hop.
  chan_id TEXT NOT NULL,

  -- The outgoing time lock of the hop.
  outgoing_time_lock INTEGER NOT NULL,

  -- The amount to forward of the hop.
  amt_to_forward BIGINT NOT NULL,

  -- The meta data of the hop.
  meta_data BLOB,

  -- Ensure unique hop per attempt
  UNIQUE(htlc_attempt_index, hop_index)
);


-- Index for duplicate route hops lookup
CREATE INDEX IF NOT EXISTS idx_duplicate_route_hops_attempt ON duplicate_payment_route_hops(htlc_attempt_index);
CREATE INDEX IF NOT EXISTS idx_duplicate_route_hops_chan_id ON duplicate_payment_route_hops(chan_id);

--- duplicate_payment_route_hop_custom_records table
CREATE TABLE IF NOT EXISTS duplicate_payment_route_hop_custom_records (
  -- The id of the custom record.
  id INTEGER PRIMARY KEY,

  -- The key of the tlv record.
  key BIGINT NOT NULL,

  -- The value of the tlv record.
  value BLOB NOT NULL,

  -- The reference to the hop.
  hop_id BIGINT NOT NULL REFERENCES payment_route_hops (id) ON DELETE CASCADE,

  -- Ensure each hop can only have one record per key.
  UNIQUE(hop_id, key)
);

CREATE INDEX IF NOT EXISTS idx_duplicate_hop_records_hop ON duplicate_payment_route_hop_custom_records(hop_id);
