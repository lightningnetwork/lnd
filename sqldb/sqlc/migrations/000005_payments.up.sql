CREATE TABLE IF NOT EXISTS payments (
    -- The id of the mpp payment.
    id BIGINT PRIMARY KEY,

    -- amount_msat is the amount of the payment in milli-satoshis.
    amount_msat BIGINT NOT NULL,

    -- payment_identifier is the identifier of the payment.
    payment_identifier BLOB NOT NULL,

    -- created_at is the timestamp of the payment.
    created_at TIMESTAMP NOT NULL
);

-- Add indexes to the payments table.


-- payment_infos table contains all the main information for a payment.
CREATE TABLE IF NOT EXISTS payment_infos (
    -- The payment_id references the payments table.
    payment_id BIGINT PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

    -- payment_status is the status of the payment.
    payment_status INTEGER NOT NULL REFERENCES payment_status_types(id) ON DELETE CASCADE
);

-- Add indexes to the payment_info table.


-- payment_requests table stores the payment request information.
CREATE TABLE IF NOT EXISTS payment_requests (
    payment_id BIGINT PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

-- not all payments have a request
    payment_request BLOB UNIQUE NOT NULL
);

-- Add indexes to the payment_request table.



-- payment_status_types stores the different types of a mpp payment.
CREATE TABLE IF NOT EXISTS payment_status_types(
    -- id is the id of the payment status type.
    id INTEGER PRIMARY KEY,

    -- description is the description of the payment status type.
    description TEXT NOT NULL
);


-- invoice_event_types defines the different types of invoice events.
INSERT INTO payment_status_types (id, description) 
VALUES 
    -- status_unknown is a deprecated status and describes the unknown payment
    -- status.
    (0, 'status_unknown'), 

    -- status_initiated is the status of a payment that has been initiated.
    (1, 'status_initiated'), 

    -- status_inflight is the status of a payment that is in flight.
    (2, 'status_inflight'),

    -- status_succeeded is the status of a payment that has been succeeded.
    (3, 'status_succeeded'),

    -- status_failed is the status of a payment that has been failed.
    (4, 'status_failed');


-- -- payment_types table stores the different types of a payment.
-- CREATE TABLE IF NOT EXISTS payment_types(
--     -- id is the id of the payment type.
--     id INTEGER PRIMARY KEY,

--     -- description is the description of the payment type.
--     description TEXT NOT NULL
-- ); 


-- Insert the payment types into the payment_types table.
-- INSERT INTO payment_types (id, description) 
-- VALUES 
--     -- legacy is a payment type that is a legacy payment.
--     (0, 'legacy'), 

--     -- mpp is a payment type that is a mpp payment.
--     (1, 'mpp'), 

--     -- amp is a payment type that is an amp payment.
--     (2, 'amp'),

--     -- blinded is a payment type that is a blinded payment.
--     (3, 'blinded');


-- -- legacy_payments table stores the legacy payments payment_hash.
-- CREATE TABLE legacy_payments (
--     payment_id INTEGER PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

--     payment_hash BLOB UNIQUE NOT NULL
-- );

-- CREATE INDEX IF NOT EXISTS legacy_payments_payment_hash_idx ON legacy_payments(payment_hash);


-- -- mpp_payments table stores stores information about the mpp payments.
-- CREATE TABLE mpp_payments (
--     payment_id INTEGER PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

--     mpp_record_id INTEGER REFERENCES mpp_record(id) ON DELETE CASCADE
-- );

-- CREATE INDEX IF NOT EXISTS mpp_payments_payment_id_idx ON mpp_payments(payment_id);
-- CREATE INDEX IF NOT EXISTS mpp_payments_mpp_record_id_idx ON mpp_payments(mpp_record_id);


-- -- amp_payments table stores stores information about the amp payments.
-- CREATE TABLE amp_payments (
--     payment_id INTEGER PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

--     amp_record_id INTEGER REFERENCES amp_record(id) ON DELETE CASCADE
-- );

-- CREATE INDEX IF NOT EXISTS amp_payments_payment_id_idx ON amp_payments(payment_id);
-- CREATE INDEX IF NOT EXISTS amp_payments_amp_record_id_idx ON amp_payments(amp_record_id);


-- payment_states table stores the state of multi path payments (amp and mpp).
CREATE TABLE payment_states (
   payment_id INTEGER PRIMARY KEY REFERENCES payments(id) ON DELETE CASCADE,

   num_attempts_in_flight INTEGER,

   remaining_amt BIGINT NOT NULL,

   fees_paid BIGINT NOT NULL,

   has_settled_htlc BOOLEAN NOT NULL,

   payment_failed BOOLEAN NOT NULL
);

CREATE INDEX IF NOT EXISTS payment_state_payment_id_idx ON payment_state(payment_id);

-- htlc_attempts table stores the htlc attempt information.
CREATE TABLE htlc_attempts (
   id INTEGER PRIMARY KEY,

   session_key BLOB UNIQUE NOT NULL,

   attempt_time TIMESTAMP NOT NULL,

   payment_id INTEGER REFERENCES payments(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS htlc_attempt_payment_id_idx ON htlc_attempt(payment_id);

-- routes table stores the route information of an htlc attempt.
CREATE TABLE routes (
   htlc_attempt_id INTEGER PRIMARY KEY REFERENCES htlc_attempt(id) ON DELETE CASCADE,

   total_timeLock  INTEGER,

   total_amount BIGINT NOT NULL,

   source_key BLOB NOT NULL,

   first_hop_amount BIGINT
);

-- hops table stores the hop information which a route is composed of.
CREATE TABLE hops (
   id INTEGER PRIMARY KEY,

   route_id INTEGER REFERENCES route(id) ON DELETE CASCADE,

   pub_key BLOB UNIQUE NOT NULL,

   chan_id TEXT UNIQUE NOT NULL,

   outgoing_time_lock INTEGER NOT NULL,

   amt_to_forward BIGINT NOT NULL,

   meta_data BLOB
);

CREATE INDEX IF NOT EXISTS hop_route_id_idx ON hop(route_id);

-- mpp_records table stores the mpp record information.
CREATE TABLE mpp_records (
 hop_id INTEGER PRIMARY KEY REFERENCES hop(id) ON DELETE CASCADE,

 payment_addr BLOB UNIQUE NOT NULL,

 total_msat BIGINT NOT NULL 
);

CREATE INDEX IF NOT EXISTS mpp_record_payment_addr_idx ON mpp_record(payment_addr);

-- amp_records table stores the amp record information.
CREATE TABLE amp_records (
   hop_id INTEGER PRIMARY KEY REFERENCES hop(id) ON DELETE CASCADE,

   root_share BLOB UNIQUE NOT NULL,

   set_id BLOB UNIQUE NOT NULL,

   child_index INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS amp_record_set_id_idx ON amp_record(set_id);

-- tlv_records table stores the tlv records for hops or payments.
CREATE TABLE tlv_records (
    id BIGINT PRIMARY KEY,

    key BIGINT NOT NULL,

    value BLOB NOT NULL
);

-- custom_records table stores the custom tlv records for a hop.
CREATE TABLE custom_records (
    tlv_record_id BIGINT PRIMARY KEY REFERENCES tlv_records(id) ON DELETE CASCADE,

    hop_id INTEGER NOT NULL REFERENCES hop(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS custom_records_tlv_record_id_idx ON custom_records(tlv_record_id );
CREATE INDEX IF NOT EXISTS custom_records_hop_id_idx ON custom_records(hop_id);

-- first_hop_custom_records table stores the first hop custom records which are
-- only send as a wire message to the first hop and are used for overlay 
-- channels.
CREATE TABLE first_hop_custom_records (
    tlv_record_id BIGINT PRIMARY KEY REFERENCES tlv_records(id) ON DELETE CASCADE,

    payment_id BIGINT NOT NULL REFERENCES payments(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS first_hop_custom_records_tlv_record_id_idx ON first_hop_custom_records(tlv_record_id);
CREATE INDEX IF NOT EXISTS first_hop_custom_records_payment_id_idx ON first_hop_custom_records(payment_id); 

-- blinded_data table stores the blinded data.
CREATE TABLE blinded_data (
  hop_id INTEGER PRIMARY KEY REFERENCES hop(id) ON DELETE CASCADE,

  encrypted_data BLOB UNIQUE NOT NULL,

  blinding_point BLOB UNIQUE,

  total_amt BIGINT NOT NULL
);

CREATE INDEX IF NOT EXISTS blinded_data_hop_id_idx ON blinded_data(hop_id);

-- htlc_settle_infos table stores the htlc settle information.
CREATE TABLE htlc_settle_infos (
   htlc_attempt_id INTEGER PRIMARY KEY REFERENCES htlc_attempts(id) ON DELETE CASCADE, 

   preimage BLOB UNIQUE NOT NULL,

   settle_time TIMESTAMP NOT NULL
);

-- htlc_fail_info table stores the htlc fail information.
CREATE TABLE htlc_fail_infos (
   htlc_attempt_id INTEGER PRIMARY KEY REFERENCES htlc_attempts(id) ON DELETE CASCADE, 

   htlc_fail_reason INTEGER NOT NULL REFERENCES htlc_fail_reason_types(id) ON DELETE CASCADE,

   failure_msg  TEXT 
);


-- htlc_fail_reason_types describes the different types of htlc fail reasons.
CREATE TABLE IF NOT EXISTS htlc_fail_reason_types(
    -- id is the id of the htlc fail reason type.
    id INTEGER PRIMARY KEY,

    -- description is the description of the htlc fail reason type.
    description TEXT NOT NULL
);

INSERT INTO htlc_fail_reason_types (id, description) 
VALUES 
    -- htlc_fail_reason_unknown is a htlc fail reason type that is unknown.
    (0, 'htlc_fail_reason_unknown'), 

    -- htlc_fail_reason_timeout is a htlc fail reason type that is a timeout.
    (1, 'htlc_fail_reason_unreadable'),

    -- htlc_fail_reason_timeout is a htlc fail reason type that is a timeout.
    (2, 'htlc_fail_reason_internal'),

    -- htlc_fail_reason_timeout is a htlc fail reason type that is a timeout.
    (3, 'htlc_fail_reason_message');

