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

