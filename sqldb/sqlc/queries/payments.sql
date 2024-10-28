-- name: GetPaymentCreation :one
SELECT *
FROM payments
WHERE payment_identifier = $1;

-- name: GetPaymentInfo :one
SELECT *
FROM payment_infos
WHERE payment_id = $1;

-- name: InsertPayment :one
INSERT INTO payments (
    payment_identifier, created_at, amount_msat
) VALUES (
    $1, $2, $3
) RETURNING id;

-- name: DeleteHTLCAttempts :exec
DELETE FROM htlc_attempts
WHERE payment_id = $1;



-- name: InsertPaymentRequest :exec
INSERT INTO payment_requests (
    payment_id, payment_request
) VALUES (
    $1, $2
);

-- name: InsertTLVRecord :one
INSERT INTO tlv_records (
    key, value
) VALUES (
    $1, $2
) RETURNING id;

-- name: InsertFirstHopCustomRecord :exec
INSERT INTO first_hop_custom_records (
    tlv_record_id, payment_id
) VALUES (
    $1, $2
);


