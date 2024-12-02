-- name: InsertInvoice :one
INSERT INTO invoices (
    hash, preimage, memo, amount_msat, cltv_delta, expiry, payment_addr, 
    payment_request, payment_request_hash, state, amount_paid_msat, is_amp,
    is_hodl, is_keysend, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
) RETURNING id;

-- name: InsertMigratedInvoice :one
INSERT INTO invoices (
    hash, preimage, settle_index, settled_at, memo, amount_msat, cltv_delta, 
    expiry, payment_addr, payment_request, payment_request_hash, state, 
    amount_paid_msat, is_amp, is_hodl, is_keysend, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17
) RETURNING id;


-- name: InsertInvoiceFeature :exec
INSERT INTO invoice_features (
    invoice_id, feature
) VALUES (
    $1, $2
);

-- name: GetInvoiceFeatures :many
SELECT *
FROM invoice_features
WHERE invoice_id = $1;

-- This method may return more than one invoice if filter using multiple fields
-- from different invoices. It is the caller's responsibility to ensure that 
-- we bubble up an error in those cases.

-- name: GetInvoice :many
SELECT i.*
FROM invoices i
LEFT JOIN amp_sub_invoices a 
ON i.id = a.invoice_id
AND (
    a.set_id = sqlc.narg('set_id') OR sqlc.narg('set_id') IS NULL
)
WHERE (
    i.id = sqlc.narg('add_index') OR 
    sqlc.narg('add_index') IS NULL
) AND (
    i.hash = sqlc.narg('hash') OR 
    sqlc.narg('hash') IS NULL
) AND (
    i.payment_addr = sqlc.narg('payment_addr') OR 
    sqlc.narg('payment_addr') IS NULL
)
GROUP BY i.id
LIMIT 2;

-- name: GetInvoiceByHash :one
SELECT i.*
FROM invoices i
WHERE i.hash = $1;

-- name: GetInvoiceBySetID :many
SELECT i.*
FROM invoices i
INNER JOIN amp_sub_invoices a 
ON i.id = a.invoice_id AND a.set_id = $1;

-- name: FilterInvoices :many
SELECT
    invoices.*
FROM invoices
WHERE (
    id >= sqlc.narg('add_index_get') OR 
    sqlc.narg('add_index_get') IS NULL
) AND (
    id <= sqlc.narg('add_index_let') OR 
    sqlc.narg('add_index_let') IS NULL
) AND (
    settle_index >= sqlc.narg('settle_index_get') OR
    sqlc.narg('settle_index_get') IS NULL
) AND (
    settle_index <= sqlc.narg('settle_index_let') OR
    sqlc.narg('settle_index_let') IS NULL
) AND (
    state = sqlc.narg('state') OR 
    sqlc.narg('state') IS NULL
) AND (
    created_at >= sqlc.narg('created_after') OR
    sqlc.narg('created_after') IS NULL
) AND (
    created_at < sqlc.narg('created_before') OR 
    sqlc.narg('created_before') IS NULL
) AND (
    CASE
        WHEN sqlc.narg('pending_only') = TRUE THEN (state = 0 OR state = 3)
        ELSE TRUE 
    END
)
ORDER BY
CASE
    WHEN sqlc.narg('reverse') = FALSE OR sqlc.narg('reverse') IS NULL THEN id
    ELSE NULL
    END ASC,
CASE
    WHEN sqlc.narg('reverse') = TRUE THEN id
    ELSE NULL
END DESC
LIMIT @num_limit OFFSET @num_offset;

-- name: UpdateInvoiceState :execresult
UPDATE invoices
SET state = $2,
    preimage = COALESCE(preimage, $3),
    settle_index = COALESCE(settle_index, $4),
    settled_at = COALESCE(settled_at, $5)
WHERE id = $1;

-- name: UpdateInvoiceAmountPaid :execresult
UPDATE invoices
SET amount_paid_msat = $2
WHERE id = $1;

-- name: NextInvoiceSettleIndex :one
UPDATE invoice_sequences SET current_value = current_value + 1
WHERE name = 'settle_index'
RETURNING current_value;

-- name: DeleteInvoice :execresult
DELETE 
FROM invoices 
WHERE (
    id = sqlc.narg('add_index') OR 
    sqlc.narg('add_index') IS NULL
) AND (
    hash = sqlc.narg('hash') OR 
    sqlc.narg('hash') IS NULL
) AND (
    settle_index = sqlc.narg('settle_index') OR 
    sqlc.narg('settle_index') IS NULL
) AND (
    payment_addr = sqlc.narg('payment_addr') OR
    sqlc.narg('payment_addr') IS NULL
);

-- name: DeleteCanceledInvoices :execresult
DELETE
FROM invoices
WHERE state = 2;

-- name: InsertInvoiceHTLC :one
INSERT INTO invoice_htlcs (
    htlc_id, chan_id, amount_msat, total_mpp_msat, accept_height, accept_time,
    expiry_height, state, resolve_time, invoice_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
) RETURNING id;

-- name: GetInvoiceHTLCs :many
SELECT *
FROM invoice_htlcs
WHERE invoice_id = $1;

-- name: UpdateInvoiceHTLC :exec
UPDATE invoice_htlcs 
SET state=$4, resolve_time=$5
WHERE htlc_id = $1 AND chan_id = $2 AND invoice_id = $3;

-- name: UpdateInvoiceHTLCs :exec
UPDATE invoice_htlcs 
SET state=$2, resolve_time=$3
WHERE invoice_id = $1 AND resolve_time IS NULL;

-- name: InsertInvoiceHTLCCustomRecord :exec
INSERT INTO invoice_htlc_custom_records (
    key, value, htlc_id 
) VALUES (
    $1, $2, $3
);

-- name: GetInvoiceHTLCCustomRecords :many
SELECT ihcr.htlc_id, key, value
FROM invoice_htlcs ih JOIN invoice_htlc_custom_records ihcr ON ih.id=ihcr.htlc_id 
WHERE ih.invoice_id = $1;

-- name: InsertKVInvoiceKeyAndAddIndex :exec
INSERT INTO invoice_payment_hashes (
    id, add_index
) VALUES (
    $1, $2
);

-- name: SetKVInvoicePaymentHash :exec
UPDATE invoice_payment_hashes
SET hash = $2
WHERE id = $1;

-- name: GetKVInvoicePaymentHashByAddIndex :one
SELECT hash
FROM invoice_payment_hashes
WHERE add_index = $1;

-- name: ClearKVInvoiceHashIndex :exec
DELETE FROM invoice_payment_hashes;
