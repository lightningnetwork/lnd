-- name: InsertInvoice :one
INSERT INTO invoices (
    hash, preimage, memo, amount_msat, cltv_delta, expiry, payment_addr, 
    payment_request, state, amount_paid_msat, is_amp, is_hodl, is_keysend, 
    created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
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

-- name: DeleteInvoiceFeatures :exec
DELETE 
FROM invoice_features
WHERE invoice_id = $1;

-- This method may return more than one invoice if filter using multiple fields
-- from different invoices. It is the caller's responsibility to ensure that 
-- we bubble up an error in those cases.

-- name: GetInvoice :many
SELECT *
FROM invoices
WHERE (
    id = sqlc.narg('add_index') OR 
    sqlc.narg('add_index') IS NULL
) AND (
    hash = sqlc.narg('hash') OR 
    sqlc.narg('hash') IS NULL
) AND (
    preimage = sqlc.narg('preimage') OR 
    sqlc.narg('preimage') IS NULL
) AND (
    payment_addr = sqlc.narg('payment_addr') OR 
    sqlc.narg('payment_addr') IS NULL
)
LIMIT 2;

-- name: FilterInvoices :many
SELECT * 
FROM invoices
WHERE (
    id >= sqlc.narg('add_index_get') OR 
    sqlc.narg('add_index_get') IS NULL
) AND (
    id <= sqlc.narg('add_index_let') OR 
    sqlc.narg('add_index_let') IS NULL
) AND (
    state = sqlc.narg('state') OR 
    sqlc.narg('state') IS NULL
) AND (
    created_at >= sqlc.narg('created_after') OR
    sqlc.narg('created_after') IS NULL
) AND (
    created_at <= sqlc.narg('created_before') OR 
    sqlc.narg('created_before') IS NULL
) AND (
    CASE
        WHEN sqlc.narg('pending_only')=TRUE THEN (state = 0 OR state = 3)
        ELSE TRUE 
    END
)
ORDER BY
    CASE
        WHEN sqlc.narg('reverse') = FALSE THEN id  
        ELSE NULL
    END ASC,
    CASE
        WHEN sqlc.narg('reverse') = TRUE  THEN id  
        ELSE NULL
    END DESC
LIMIT @num_limit OFFSET @num_offset;

-- name: UpdateInvoice :exec 
UPDATE invoices 
SET preimage=$2, state=$3, amount_paid_msat=$4
WHERE id=$1;

-- name: DeleteInvoice :exec
DELETE 
FROM invoices 
WHERE (
    id = sqlc.narg('add_index') OR 
    sqlc.narg('add_index') IS NULL
) AND (
    hash = sqlc.narg('hash') OR 
    sqlc.narg('hash') IS NULL
) AND (
    preimage = sqlc.narg('preimage') OR 
    sqlc.narg('preimage') IS NULL
) AND (
    payment_addr = sqlc.narg('payment_addr') OR
    sqlc.narg('payment_addr') IS NULL
);

-- name: InsertInvoiceHTLC :exec
INSERT INTO invoice_htlcs (
    htlc_id, chan_id, amount_msat, total_mpp_msat, accept_height, accept_time,
    expiry_height, state, resolve_time, invoice_id
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
);

-- name: GetInvoiceHTLCs :many
SELECT *
FROM invoice_htlcs
WHERE invoice_id = $1;

-- name: UpdateInvoiceHTLC :exec
UPDATE invoice_htlcs 
SET state=$2, resolve_time=$3
WHERE id = $1;

-- name: UpdateInvoiceHTLCs :exec
UPDATE invoice_htlcs 
SET state=$2, resolve_time=$3
WHERE invoice_id = $1 AND resolve_time IS NULL;


-- name: DeleteInvoiceHTLC :exec
DELETE
FROM invoice_htlcs
WHERE htlc_id = $1;

-- name: DeleteInvoiceHTLCs :exec
DELETE
FROM invoice_htlcs
WHERE invoice_id = $1;

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

-- name: DeleteInvoiceHTLCCustomRecords :exec
WITH htlc_ids AS (
    SELECT ih.id
    FROM invoice_htlcs ih JOIN invoice_htlc_custom_records ihcr ON ih.id=ihcr.htlc_id 
    WHERE ih.invoice_id = $1
)
DELETE
FROM invoice_htlc_custom_records
WHERE htlc_id IN (SELECT id FROM htlc_ids);

-- name: InsertInvoicePayment :one
INSERT INTO invoice_payments (
    invoice_id, amount_paid_msat, settled_at 
) VALUES (
    $1, $2, $3
) RETURNING id;

-- name: GetInvoicePayments :many
SELECT *
FROM invoice_payments
WHERE invoice_id = $1;

-- name: FilterInvoicePayments :many
SELECT  
    ip.id AS settle_index, ip.amount_paid_msat, ip.settled_at AS settle_date, 
    i.* 
FROM invoice_payments ip JOIN invoices i ON ip.invoice_id = i.id
WHERE (
    ip.id >= sqlc.narg('settle_index_get') OR 
    sqlc.narg('settle_index_get') IS NULL
) AND (
    ip.settled_at >= sqlc.narg('settled_after') OR 
    sqlc.narg('settled_after') IS NULL
)
ORDER BY
    CASE
        WHEN sqlc.narg('reverse') = FALSE THEN ip.id  
        ELSE NULL
    END ASC,
    CASE
        WHEN sqlc.narg('reverse') = TRUE THEN ip.id  
        ELSE NULL
    END DESC
LIMIT @num_limit OFFSET @num_offset;

