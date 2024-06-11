-- name: UpsertAMPSubInvoice :execresult
INSERT INTO amp_sub_invoices (
    set_id, state, created_at, invoice_id
) VALUES (
    $1, $2, $3, $4
) ON CONFLICT (set_id, invoice_id) DO NOTHING;

-- name: GetAMPInvoiceID :one
SELECT invoice_id FROM amp_sub_invoices WHERE set_id = $1;

-- name: UpdateAMPSubInvoiceState :exec
UPDATE amp_sub_invoices
SET state = $2, 
    settle_index = COALESCE(settle_index, $3),
    settled_at = COALESCE(settled_at, $4)
WHERE set_id = $1; 

-- name: InsertAMPSubInvoiceHTLC :exec
INSERT INTO amp_sub_invoice_htlcs (
    invoice_id, set_id, htlc_id, root_share, child_index, hash, preimage
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
);

-- name: FetchAMPSubInvoices :many
SELECT *
FROM amp_sub_invoices
WHERE invoice_id = $1 
AND (
    set_id = sqlc.narg('set_id') OR 
    sqlc.narg('set_id') IS NULL
);

-- name: FetchAMPSubInvoiceHTLCs :many
SELECT 
    amp.set_id, amp.root_share, amp.child_index, amp.hash, amp.preimage, 
    invoice_htlcs.*
FROM amp_sub_invoice_htlcs amp
INNER JOIN invoice_htlcs ON amp.htlc_id = invoice_htlcs.id
WHERE amp.invoice_id = $1
AND (
    set_id = sqlc.narg('set_id') OR 
    sqlc.narg('set_id') IS NULL
);

-- name: FetchSettledAMPSubInvoices :many
SELECT 
    a.set_id, 
    a.settle_index as amp_settle_index, 
    a.settled_at as amp_settled_at,
    i.*
FROM amp_sub_invoices a
INNER JOIN invoices i ON a.invoice_id = i.id
WHERE (
    a.settle_index >= sqlc.narg('settle_index_get') OR
    sqlc.narg('settle_index_get') IS NULL
) AND (
    a.settle_index <= sqlc.narg('settle_index_let') OR
    sqlc.narg('settle_index_let') IS NULL
);

-- name: UpdateAMPSubInvoiceHTLCPreimage :execresult
UPDATE amp_sub_invoice_htlcs AS a
SET preimage = $5
WHERE a.invoice_id = $1 AND a.set_id = $2 AND a.htlc_id = (
    SELECT id FROM invoice_htlcs AS i WHERE i.chan_id = $3 AND i.htlc_id = $4
);

-- name: InsertAMPSubInvoice :exec
INSERT INTO amp_sub_invoices (
    set_id, state, created_at, settled_at, settle_index, invoice_id
) VALUES (
    $1, $2, $3, $4, $5, $6
);

