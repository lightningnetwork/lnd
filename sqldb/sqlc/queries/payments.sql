/* ─────────────────────────────────────────────
   fetch queries
   ─────────────────────────────────────────────
*/

-- name: FilterPayments :many
SELECT * FROM payments
WHERE (
    -- This will exclude payments which have the failed reason set. These 
    -- payments might not be final yet meaning that they still can be inflight
    -- but they will transition to failed when all corresponding HTLCs are
    -- resolved.     
    (sqlc.narg('exclude_failed') = true AND fail_reason IS NULL) OR
    sqlc.narg('exclude_failed') = false OR sqlc.narg('exclude_failed') IS NULL
)
 AND (
    id > sqlc.narg('index_offset_get') OR
    sqlc.narg('index_offset_get') IS NULL 

) AND (
    id < sqlc.narg('index_offset_let') OR
    sqlc.narg('index_offset_let') IS NULL
) AND (
    created_at >= sqlc.narg('created_after') OR
    sqlc.narg('created_after') IS NULL
) AND (
    created_at <= sqlc.narg('created_before') OR
    sqlc.narg('created_before') IS NULL
)
ORDER BY 
    CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN id END ASC,
    CASE WHEN sqlc.narg('reverse') = true THEN id END DESC
LIMIT @num_limit;

-- name: FetchPayment :one
SELECT * FROM payments WHERE payment_hash = $1;


-- name: CountPayments :one
SELECT COUNT(*) FROM payments;


-- name: FetchHtlcAttempts :many
-- This fetches all htlc attempts for a payment.
SELECT * FROM payment_htlc_attempts ha
WHERE ha.payment_id = $1
    AND (
        (sqlc.narg('in_flight_only') = true AND ha.settle_preimage IS NULL AND ha.htlc_fail_reason IS NULL)
        OR
        (sqlc.narg('in_flight_only') = false OR sqlc.narg('in_flight_only') IS NULL)
    )
ORDER BY
    CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN ha.attempt_time END ASC,
    CASE WHEN sqlc.narg('reverse') = true THEN ha.attempt_time END DESC;

-- name: FetchAllInflightAttempts :many
-- Fetch all inflight attempts across all payments
SELECT * FROM payment_htlc_attempts ha
WHERE ha.settle_preimage IS NULL AND ha.htlc_fail_reason IS NULL;


-- name: FetchHopsForAttempt :many
SELECT * FROM payment_route_hops h
WHERE h.htlc_attempt_index = $1
ORDER BY h.hop_index ASC;

-- name: FetchHopsForAttempts :many
SELECT * FROM payment_route_hops
WHERE htlc_attempt_index IN (sqlc.slice('htlc_attempt_indices')/*SLICE:htlc_attempt_indices*/);

-- name: FetchCustomRecordsForHops :many
SELECT * FROM payment_route_hop_custom_records
WHERE hop_id IN (sqlc.slice('hop_ids')/*SLICE:hop_ids*/);

-- name: FetchFirstHopCustomRecords :many
SELECT * FROM payment_first_hop_custom_records WHERE payment_id = $1;

-- name: FetchCustomRecordsForAttempts :many
SELECT * FROM payment_htlc_attempt_custom_records
WHERE htlc_attempt_index IN (sqlc.slice('htlc_attempt_indices')/*SLICE:htlc_attempt_indices*/);


