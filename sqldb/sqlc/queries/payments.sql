/* ─────────────────────────────────────────────
   fetch queries
   ─────────────────────────────────────────────
*/

-- name: FilterPayments :many
SELECT
    sqlc.embed(p),
    i.intent_type AS "intent_type",
    i.intent_payload AS "intent_payload"
FROM payments p
LEFT JOIN payment_intents i ON i.payment_id = p.id
WHERE (
    p.id > sqlc.narg('index_offset_get') OR
    sqlc.narg('index_offset_get') IS NULL
) AND (
    p.id < sqlc.narg('index_offset_let') OR
    sqlc.narg('index_offset_let') IS NULL
) AND (
    p.created_at >= sqlc.narg('created_after') OR
    sqlc.narg('created_after') IS NULL
) AND (
    p.created_at <= sqlc.narg('created_before') OR
    sqlc.narg('created_before') IS NULL
) AND (
    i.intent_type = sqlc.narg('intent_type') OR
    sqlc.narg('intent_type') IS NULL OR i.intent_type IS NULL
)
ORDER BY
    CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN p.id END ASC,
    CASE WHEN sqlc.narg('reverse') = true THEN p.id END DESC
LIMIT @num_limit;

-- name: FetchPayment :one
SELECT
    sqlc.embed(p),
    i.intent_type AS "intent_type",
    i.intent_payload AS "intent_payload"
FROM payments p
LEFT JOIN payment_intents i ON i.payment_id = p.id
WHERE p.payment_identifier = $1;

-- name: FetchPaymentsByIDs :many
SELECT
    sqlc.embed(p),
    i.intent_type AS "intent_type",
    i.intent_payload AS "intent_payload"
FROM payments p
LEFT JOIN payment_intents i ON i.payment_id = p.id
WHERE p.id IN (sqlc.slice('payment_ids')/*SLICE:payment_ids*/);

-- name: CountPayments :one
SELECT COUNT(*) FROM payments;

-- name: FetchHtlcAttemptsForPayments :many
SELECT
    ha.id,
    ha.attempt_index,
    ha.payment_id,
    ha.session_key,
    ha.attempt_time,
    ha.payment_hash,
    ha.first_hop_amount_msat,
    ha.route_total_time_lock,
    ha.route_total_amount,
    ha.route_source_key,
    hr.resolution_type,
    hr.resolution_time,
    hr.failure_source_index,
    hr.htlc_fail_reason,
    hr.failure_msg,
    hr.settle_preimage
FROM payment_htlc_attempts ha
LEFT JOIN payment_htlc_attempt_resolutions hr ON hr.attempt_index = ha.attempt_index
WHERE ha.payment_id IN (sqlc.slice('payment_ids')/*SLICE:payment_ids*/)
ORDER BY ha.payment_id ASC, ha.attempt_time ASC;

-- name: FetchHtlcAttemptResolutionsForPayment :many
-- Lightweight query to fetch only HTLC resolution status.
SELECT
    hr.resolution_type
FROM payment_htlc_attempts ha
LEFT JOIN payment_htlc_attempt_resolutions hr ON hr.attempt_index = ha.attempt_index
WHERE ha.payment_id = $1
ORDER BY ha.attempt_time ASC;

-- name: FetchAllInflightAttempts :many
-- Fetch all inflight attempts across all payments
SELECT
    ha.id,
    ha.attempt_index,
    ha.payment_id,
    ha.session_key,
    ha.attempt_time,
    ha.payment_hash,
    ha.first_hop_amount_msat,
    ha.route_total_time_lock,
    ha.route_total_amount,
    ha.route_source_key
FROM payment_htlc_attempts ha
WHERE NOT EXISTS (
    SELECT 1 FROM payment_htlc_attempt_resolutions hr
    WHERE hr.attempt_index = ha.attempt_index
)
ORDER BY ha.attempt_index ASC;

-- name: FetchHopsForAttempts :many
SELECT
    h.id,
    h.htlc_attempt_index,
    h.hop_index,
    h.pub_key,
    h.scid,
    h.outgoing_time_lock,
    h.amt_to_forward,
    h.meta_data,
    m.payment_addr AS mpp_payment_addr,
    m.total_msat AS mpp_total_msat,
    a.root_share AS amp_root_share,
    a.set_id AS amp_set_id,
    a.child_index AS amp_child_index,
    b.encrypted_data,
    b.blinding_point,
    b.blinded_path_total_amt
FROM payment_route_hops h
LEFT JOIN payment_route_hop_mpp m ON m.hop_id = h.id
LEFT JOIN payment_route_hop_amp a ON a.hop_id = h.id
LEFT JOIN payment_route_hop_blinded b ON b.hop_id = h.id
WHERE h.htlc_attempt_index IN (sqlc.slice('htlc_attempt_indices')/*SLICE:htlc_attempt_indices*/)
ORDER BY h.htlc_attempt_index ASC, h.hop_index ASC;


-- name: FetchPaymentLevelFirstHopCustomRecords :many
SELECT
    l.id,
    l.payment_id,
    l.key,
    l.value
FROM payment_first_hop_custom_records l
WHERE l.payment_id IN (sqlc.slice('payment_ids')/*SLICE:payment_ids*/)
ORDER BY l.payment_id ASC, l.key ASC;

-- name: FetchRouteLevelFirstHopCustomRecords :many
SELECT
    l.id,
    l.htlc_attempt_index,
    l.key,
    l.value
FROM payment_attempt_first_hop_custom_records l
WHERE l.htlc_attempt_index IN (sqlc.slice('htlc_attempt_indices')/*SLICE:htlc_attempt_indices*/)
ORDER BY l.htlc_attempt_index ASC, l.key ASC;

-- name: FetchHopLevelCustomRecords :many
SELECT
    l.id,
    l.hop_id,
    l.key,
    l.value
FROM payment_hop_custom_records l
WHERE l.hop_id IN (sqlc.slice('hop_ids')/*SLICE:hop_ids*/)
ORDER BY l.hop_id ASC, l.key ASC;


-- name: DeletePayment :exec
DELETE FROM payments WHERE id = $1;

-- name: DeleteFailedAttempts :exec
-- Delete all failed HTLC attempts for the given payment. Resolution type 2
-- indicates a failed attempt.
DELETE FROM payment_htlc_attempts WHERE payment_id = $1 AND attempt_index IN (
    SELECT attempt_index FROM payment_htlc_attempt_resolutions WHERE resolution_type = 2
);

-- name: InsertPaymentIntent :one
-- Insert a payment intent for a given payment and return its ID.
INSERT INTO payment_intents (
    payment_id,
    intent_type, 
    intent_payload)
VALUES (
    @payment_id,
    @intent_type, 
    @intent_payload
)
RETURNING id;

-- name: InsertPayment :one
-- Insert a new payment and return its ID.
-- When creating a payment we don't have a fail reason because we start the
-- payment process.
INSERT INTO payments (
    amount_msat, 
    created_at, 
    payment_identifier,
    fail_reason)
VALUES (
    @amount_msat,
    @created_at,
    @payment_identifier,
    NULL
)
RETURNING id;

-- name: InsertPaymentFirstHopCustomRecord :exec
INSERT INTO payment_first_hop_custom_records (
    payment_id,
    key,
    value
)
VALUES (
    @payment_id,
    @key,
    @value
);

-- name: InsertHtlcAttempt :one
INSERT INTO payment_htlc_attempts (
    payment_id,
    attempt_index,
    session_key,
    attempt_time,
    payment_hash,
    first_hop_amount_msat,
    route_total_time_lock,
    route_total_amount,
    route_source_key)
VALUES (
    @payment_id,
    @attempt_index,
    @session_key,
    @attempt_time,
    @payment_hash, 
    @first_hop_amount_msat, 
    @route_total_time_lock, 
    @route_total_amount, 
    @route_source_key)
RETURNING id;

-- name: InsertPaymentAttemptFirstHopCustomRecord :exec
INSERT INTO payment_attempt_first_hop_custom_records (
    htlc_attempt_index,
    key,
    value
)
VALUES (
    @htlc_attempt_index,
    @key,
    @value
);

-- name: InsertRouteHop :one
INSERT INTO payment_route_hops (
    htlc_attempt_index,
    hop_index,
    pub_key,
    scid,
    outgoing_time_lock,
    amt_to_forward,
    meta_data
)
VALUES (
    @htlc_attempt_index,
    @hop_index,
    @pub_key,
    @scid,
    @outgoing_time_lock,
    @amt_to_forward,
    @meta_data
)
RETURNING id;

-- name: InsertRouteHopMpp :exec
INSERT INTO payment_route_hop_mpp (
    hop_id,
    payment_addr,
    total_msat
)
VALUES (
    @hop_id,
    @payment_addr,
    @total_msat
);

-- name: InsertRouteHopAmp :exec
INSERT INTO payment_route_hop_amp (
    hop_id,
    root_share,
    set_id,
    child_index
)
VALUES (
    @hop_id,
    @root_share,
    @set_id,
    @child_index
);

-- name: InsertRouteHopBlinded :exec
INSERT INTO payment_route_hop_blinded (
    hop_id,
    encrypted_data,
    blinding_point,
    blinded_path_total_amt
)
VALUES (
    @hop_id,
    @encrypted_data,
    @blinding_point,
    @blinded_path_total_amt
);

-- name: InsertPaymentHopCustomRecord :exec
INSERT INTO payment_hop_custom_records (
    hop_id,
    key,
    value
)
VALUES (
    @hop_id,
    @key,
    @value
);

-- name: SettleAttempt :exec
INSERT INTO payment_htlc_attempt_resolutions (
    attempt_index,
    resolution_time,
    resolution_type,
    settle_preimage
)
VALUES (
    @attempt_index,
    @resolution_time,
    @resolution_type,
    @settle_preimage
);

-- name: FailAttempt :exec
INSERT INTO payment_htlc_attempt_resolutions (
    attempt_index,
    resolution_time,
    resolution_type,
    failure_source_index,
    htlc_fail_reason,
    failure_msg
)
VALUES (
    @attempt_index,
    @resolution_time,
    @resolution_type,
    @failure_source_index,
    @htlc_fail_reason,
    @failure_msg
);

-- name: FailPayment :execresult
UPDATE payments SET fail_reason = $1 WHERE payment_identifier = $2;
