-- name: InsertAMPInvoicePayment :exec
INSERT INTO amp_invoice_payments (
    set_id, state, created_at, settled_index, invoice_id
) VALUES (
    $1, $2, $3, $4, $5
); 

-- name: SelectAMPInvoicePayments :many
SELECT aip.*, ip.*
FROM amp_invoice_payments aip LEFT JOIN invoice_payments ip ON aip.settled_index = ip.id
WHERE (
    set_id = sqlc.narg('set_id') OR 
    sqlc.narg('set_id') IS NULL
) AND (
    aip.settled_index = sqlc.narg('settled_index') OR 
    sqlc.narg('settled_index') IS NULL
) AND (
    aip.invoice_id = sqlc.narg('invoice_id') OR 
    sqlc.narg('invoice_id') IS NULL
); 

-- name: UpdateAMPPayment :exec
UPDATE amp_invoice_payments
SET state = $1, settled_index = $2
WHERE state = 0 AND (
    set_id = sqlc.narg('set_id') OR 
    sqlc.narg('set_id') IS NULL
) AND (
    invoice_id = sqlc.narg('invoice_id') OR 
    sqlc.narg('invoice_id') IS NULL
); 

-- name: InsertAMPInvoiceHTLC :exec
INSERT INTO amp_invoice_htlcs (
    set_id, htlc_id, root_share, child_index, hash, preimage
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: GetAMPInvoiceHTLCsBySetID :many
SELECT *
FROM amp_invoice_htlcs
WHERE set_id = $1;

-- name: GetAMPInvoiceHTLCsByInvoiceID :many
SELECT *
FROM amp_invoice_htlcs
WHERE invoice_id = $1;

-- name: GetSetIDHTLCsCustomRecords :many
SELECT ihcr.htlc_id, key, value
FROM amp_invoice_htlcs aih JOIN invoice_htlc_custom_records ihcr ON aih.id=ihcr.htlc_id 
WHERE aih.set_id = $1;

-- name: UpdateAMPInvoiceHTLC :exec
UPDATE amp_invoice_htlcs
SET preimage = $1
WHERE htlc_id = $2;

-- name: DeleteAMPHTLCCustomRecords :exec
WITH htlc_ids AS (
    SELECT htlc_id
    FROM amp_invoice_htlcs 
    WHERE invoice_id = $1
)
DELETE
FROM invoice_htlc_custom_records
WHERE htlc_id IN (SELECT id FROM htlc_ids);

-- name: DeleteAMPHTLCs :exec
DELETE 
FROM amp_invoice_htlcs  
WHERE invoice_id = $1;

-- name: DeleteAMPInvoiceHTLC :exec
DELETE 
FROM amp_invoice_htlcs
WHERE set_id = $1;

