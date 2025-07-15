/* ─────────────────────────────────────────────
   fetch queries
   ─────────────────────────────────────────────
*/

-- name: FilterPayments :many
SELECT * FROM payments
WHERE (
    -- This will include payments which have the failed reason set. This means
    -- that they are not settled however they are still not necessarily failed
    -- because they are still in flight but we know for sure that if they are
    -- currently in flight that they will transition to failed. This helps us
    -- to not fetch payments that we know are failed or will transition to 
    -- failed.
    (sqlc.narg('exclude_failed') = true AND fail_reason IS NULL) OR
    sqlc.narg('exclude_failed') = false OR sqlc.narg('exclude_failed') IS NULL
) AND (
    -- Optional cursor-based pagination.
    sqlc.narg('index_offset_get') IS NULL OR
    id >= sqlc.narg('index_offset_get')
) AND (
    id <= sqlc.narg('index_offset_let') OR
    sqlc.narg('index_offset_let') IS NULL
) AND (
    -- Optional date filters.
    (sqlc.narg('created_after') IS NULL) OR
    (sqlc.narg('created_after') IS NOT NULL AND created_at >= sqlc.narg('created_after'))
) AND (
    created_at < sqlc.narg('created_before') OR
    sqlc.narg('created_before') IS NULL
)
ORDER BY 
    CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN id END ASC,
    CASE WHEN sqlc.narg('reverse') = true THEN id END DESC
LIMIT @num_limit OFFSET @num_offset;

-- name: FetchDuplicatePayments :many
SELECT * FROM duplicate_payments
WHERE payment_hash = $1
ORDER BY created_at ASC;


-- name: FetchHtlcAttempts :many
-- This will not include the hops for the htlc attempts which can be fetched
-- with the FetchHopsForAttempt query.
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

-- name: FetchDuplicateHtlcAttempts :many
-- Fetch HTLC attempts for duplicate payments
SELECT * FROM duplicate_payment_htlc_attempts ha
WHERE ha.duplicate_payment_id = $1 
    AND (
        (sqlc.narg('in_flight_only') = true AND ha.settle_preimage IS NULL AND ha.htlc_fail_reason IS NULL)
        OR
        (sqlc.narg('in_flight_only') = false OR sqlc.narg('in_flight_only') IS NULL)
    )
ORDER BY 
    CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN ha.attempt_time END ASC,
    CASE WHEN sqlc.narg('reverse') = true THEN ha.attempt_time END DESC;

-- name: FetchHopsForAttempt :many
SELECT * FROM payment_route_hops h
WHERE h.htlc_attempt_index = $1
ORDER BY h.hop_index ASC;

-- name: FetchDuplicateHopsForAttempt :many
SELECT * FROM duplicate_payment_route_hops h
WHERE h.htlc_attempt_index = $1
ORDER BY h.hop_index ASC;

-- name: FetchFirstHopCustomRecords :many
SELECT * FROM payment_first_hop_custom_records WHERE payment_id = $1;

-- name: FetchCustomRecordsForHop :many
SELECT * FROM payment_route_hop_custom_records WHERE hop_id = $1;

-- name: FetchDuplicateHopCustomRecordsForHop :many
SELECT * FROM duplicate_payment_route_hop_custom_records WHERE hop_id = $1;

-- name: FetchPayment :one
SELECT * FROM payments WHERE payment_hash = $1;

-- name: FetchInflightHTLCAttempts :many
SELECT * FROM payment_htlc_attempts WHERE  settle_preimage IS NULL AND htlc_fail_reason IS NULL;

/* ─────────────────────────────────────────────
   delete queries
   ─────────────────────────────────────────────
*/

-- name: DeletePayment :exec
DELETE FROM payments WHERE payment_hash = $1;

-- name: DeletePaymentByID :exec
DELETE FROM payments WHERE id = $1;

-- name: DeleteHTLCAttempt :exec
-- Delete a single HTLC attempt by its index.
DELETE FROM payment_htlc_attempts WHERE attempt_index = $1;

-- name: DeleteFailedAttempts :exec
DELETE FROM payment_htlc_attempts WHERE payment_id = $1 AND htlc_fail_reason IS NOT NULL;

/* ─────────────────────────────────────────────
   insert queries
   ─────────────────────────────────────────────
*/

-- name: InsertPayment :one
INSERT INTO payments (
    payment_type, 
    payment_request, 
    amount_msat, 
    created_at,
    payment_hash
) VALUES (
    @payment_type, @payment_request, 
    @amount_msat, @created_at, @payment_hash
) RETURNING id;

-- name: InsertDuplicatePayment :one
INSERT INTO duplicate_payments (
    payment_hash,
    payment_request,
    amount_msat,
    created_at,
    fail_reason
) VALUES (
    @payment_hash, @payment_request,
    @amount_msat, @created_at, @fail_reason
) RETURNING id;

-- name: InsertFirstHopCustomRecord :exec
INSERT INTO payment_first_hop_custom_records (
    payment_id,
    key,
    value
) VALUES (@payment_id, @key, @value);

-- name: InsertHtlcAttempt :one
INSERT INTO payment_htlc_attempts (
    attempt_index,
    payment_id,
    payment_hash,
    attempt_time,
    session_key,
    route_total_timeLock,
    route_total_amount,
    route_first_hop_amount,
    route_source_key
) VALUES (@attempt_index, @payment_id, @payment_hash, @attempt_time, 
@session_key, @route_total_timelock, @route_total_amount, 
@route_first_hop_amount, @route_source_key) RETURNING id;

-- name: InsertDuplicateHtlcAttempt :one
INSERT INTO duplicate_payment_htlc_attempts (
    attempt_index,
    duplicate_payment_id,
    payment_hash,
    attempt_time,
    session_key,
    route_total_timeLock,
    route_total_amount,
    route_source_key
) VALUES (@attempt_index, @duplicate_payment_id, @payment_hash, @attempt_time, 
@session_key, @route_total_timelock, @route_total_amount, 
@route_source_key) RETURNING id;

-- name: InsertHop :one
INSERT INTO payment_route_hops (
    htlc_attempt_index,
    hop_index,
    pub_key,
    chan_id,
    outgoing_time_lock,
    amt_to_forward,
    meta_data,
    legacy_payload,
    mpp_payment_addr,
    mpp_total_msat,
    amp_root_share,
    amp_set_id,
    amp_child_index,
    encrypted_data,
    blinding_point,
    blinded_path_total_amt
) VALUES (@htlc_attempt_index, @hop_index, @pub_key, @chan_id,
 @outgoing_time_lock, @amt_to_forward, @meta_data, @legacy_payload,
@mpp_payment_addr, @mpp_total_msat, @amp_root_share, @amp_set_id,
@amp_child_index, @encrypted_data, @blinding_point, 
@blinded_path_total_amt) RETURNING id;

-- name: InsertDuplicateHop :one
INSERT INTO duplicate_payment_route_hops (
    htlc_attempt_index,
    hop_index,
    pub_key,
    chan_id,
    outgoing_time_lock,
    amt_to_forward,
    meta_data
) VALUES (@htlc_attempt_index, @hop_index, @pub_key, @chan_id,
 @outgoing_time_lock, @amt_to_forward, @meta_data) RETURNING id;

-- name: InsertHopCustomRecord :exec
INSERT INTO payment_route_hop_custom_records (
    hop_id,
    key,
    value
) VALUES (@hop_id, @key, @value);

-- name: InsertDuplicateHopCustomRecord :exec
INSERT INTO duplicate_payment_route_hop_custom_records (
    hop_id,
    key,
    value
) VALUES (@hop_id, @key, @value);

/* ─────────────────────────────────────────────
   update queries
   ─────────────────────────────────────────────
*/

-- name: UpdateHtlcAttemptSettleInfo :one
UPDATE payment_htlc_attempts 
SET settle_preimage = @settle_preimage, settle_time = @settle_time 
WHERE attempt_index = @attempt_index
RETURNING id;

-- name: UpdateHtlcAttemptFailInfo :one 
UPDATE payment_htlc_attempts 
SET failure_source_index = @failure_source_index, 
htlc_fail_reason = @htlc_fail_reason, failure_msg = @failure_msg,
 fail_time = @fail_time
WHERE attempt_index = @attempt_index
RETURNING id;

-- name: UpdatePaymentFailReason :one
UPDATE payments 
SET fail_reason = @fail_reason
WHERE payment_hash = @payment_hash
RETURNING id;

/* ─────────────────────────────────────────────
   sequence number queries
   ─────────────────────────────────────────────
*/

-- name: NextHtlcAttemptIndex :one
UPDATE payment_sequences 
SET current_value = current_value + 1 
WHERE name = 'htlc_attempt_index' 
RETURNING current_value;














-- -- name: InsertPayment :one
-- INSERT INTO payments (
--     payment_type, 
--     -- Optional: can be NULL when paying directly to a hash.
--     payment_request, 
--     amount_msat, 
--     created_at,
--     payment_hash
-- ) VALUES (
--     @payment_type, @payment_request, 
--     @amount_msat, @created_at, @payment_hash
-- ) RETURNING id;




-- -- name: FetchPayment :one
-- SELECT * FROM payments WHERE payment_hash = @payment_hash;


-- -- name: InsertFirstHopCustomRecord :exec
-- INSERT INTO payment_first_hop_custom_records (
--     payment_id,
--     key,
--     value
-- ) VALUES (@payment_id, @key, @value);



-- -- name: UpdatePaymentFailReason :one
-- UPDATE payments 
-- SET fail_reason = $2 
-- WHERE payment_hash = $1
-- RETURNING id;




-- -- name: FetchSingleHtlcAttempt :one
-- SELECT * FROM payment_htlc_attempts WHERE attempt_index = $1;



-- -- name: FetchCustomRecordsForHop :many
-- SELECT * FROM payment_route_hop_custom_records WHERE hop_id = $1;

-- -- name: UpdateHtlcAttemptSettleInfo :one
-- UPDATE payment_htlc_attempts 
-- SET settle_preimage = $2, settle_time = $3 
-- WHERE attempt_index = $1
-- RETURNING id;

-- -- name: UpdateHtlcAttemptFailInfo :one
-- UPDATE payment_htlc_attempts 
-- SET htlc_fail_reason = $2, failure_msg = $3 
-- WHERE attempt_index = $1
-- RETURNING id;

-- -- name: DeletePayment :exec
-- DELETE FROM payments WHERE payment_hash = $1;

-- -- name: DeletePayments :execresult
-- DELETE FROM payments WHERE payment_status = $1;

-- -- name: DeleteAllPayments :execresult
-- -- Delete all payments except in-flight ones (status != StatusInFlight)
-- DELETE FROM payments where payment_status != 2;

-- -- name: DeleteAllFailedAttempts :execresult
-- DELETE FROM payment_htlc_attempts WHERE  htlc_fail_reason IS NOT NULL;

-- -- name: DeleteFailedAttempts :execresult
-- DELETE FROM payment_htlc_attempts WHERE payment_id = $1 AND htlc_fail_reason IS NOT NULL;

-- -- name: InsertHtlcAttempt :one
-- INSERT INTO payment_htlc_attempts (
--     attempt_index,
--     payment_id,
--     payment_hash,
--     attempt_time,
--     session_key,
--     route_total_timeLock,
--     route_total_amount,
--     route_first_hop_amount,
--     route_source_key
-- ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id;

-- -- name: InsertHop :one
-- INSERT INTO payment_route_hops (
--     htlc_attempt_index,
--     hop_index,
--     pub_key,
--     chan_id,
--     outgoing_time_lock,
--     amt_to_forward,
--     meta_data,
--     mpp_payment_addr,
--     mpp_total_msat,
--     amp_root_share,
--     amp_set_id,
--     amp_child_index,
--     encrypted_data,
--     blinding_point,
--     blinded_path_total_amt
-- ) VALUES (
--     $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
-- ) RETURNING id;

-- -- name: NextHtlcAttemptIndex :one
-- -- Get the next HTLC attempt ID by incrementing the counter.
-- UPDATE payment_sequences 
-- SET current_value = current_value + 1 
-- WHERE name = 'htlc_attempt_index' 
-- RETURNING current_value;

-- -- name: FilterPayments :many
-- SELECT * FROM payments
-- WHERE (
--     -- This will include payments which have the failed reason set. This means
--     -- that they are not settled however they are still not necessarily failed
--     -- because they are still in flight but we know for sure that if they are
--     -- currently in flight that they will transition to failed.
--     sqlc.narg('exclude_failed') IS NULL OR 
--     sqlc.narg('exclude_failed') = false OR
--     (sqlc.narg('exclude_failed') = true AND fail_reason IS NULL)
-- ) AND (
--     -- Optional cursor-based pagination.
--     sqlc.narg('index_offset_get') IS NULL OR
--     id >= sqlc.narg('index_offset_get')
-- ) AND (
--     id <= sqlc.narg('index_offset_let') OR
--     sqlc.narg('index_offset_let') IS NULL
-- ) AND (
--     -- Optional date filters.
--     (sqlc.narg('created_after') IS NULL) OR
--     (sqlc.narg('created_after') IS NOT NULL AND created_at >= sqlc.narg('created_after'))
-- ) AND (
--     created_at < sqlc.narg('created_before') OR
--     sqlc.narg('created_before') IS NULL
-- )
-- ORDER BY 
--     CASE WHEN sqlc.narg('reverse') = false OR sqlc.narg('reverse') IS NULL THEN id END ASC,
--     CASE WHEN sqlc.narg('reverse') = true THEN id END DESC
-- LIMIT @num_limit OFFSET @num_offset;





-- -- name: FetchAllCustomRecordsForPayment :many
-- SELECT 
--     'first_hop' as record_type,
--     fh.id,
--     fh.key,
--     fh.value,
--     fh.payment_id,
--     NULL::bigint as hop_id
-- FROM payment_first_hop_custom_records fh
-- WHERE fh.payment_id = $1
-- UNION ALL
-- SELECT 
--     'hop' as record_type,
--     cr.id,
--     cr.key,
--     cr.value,
--     ha.payment_id,
--     cr.hop_id
-- FROM payment_route_hop_custom_records cr
-- JOIN payment_route_hops h ON cr.hop_id = h.id
-- JOIN payment_htlc_attempts ha ON h.htlc_attempt_index = ha.attempt_index
-- WHERE ha.payment_id = $1;

-- -- name: FetchPaymentSummaries :many
-- SELECT 
--     p.id,
--     p.payment_status,
--     p.payment_type,
--     p.amount_msat,
--     p.created_at,
--     p.payment_hash,
--     p.fail_reason,
--     COUNT(ha.id) as attempt_count,
--     SUM(CASE WHEN ha.settle_preimage IS NOT NULL THEN 1 ELSE 0 END) as settled_count,
--     SUM(CASE WHEN ha.htlc_fail_reason IS NOT NULL THEN 1 ELSE 0 END) as failed_count
-- FROM payments p
-- LEFT JOIN payment_htlc_attempts ha ON p.id = ha.payment_id
-- WHERE p.payment_status = $1
-- GROUP BY p.id, p.payment_status, p.payment_type, p.amount_msat, p.created_at, p.payment_hash, p.fail_reason
-- ORDER BY p.id
-- LIMIT $2 OFFSET $3;

















