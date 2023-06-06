-- name: InsertInvoiceEvent :exec
INSERT INTO invoice_events (
    created_at, invoice_id, htlc_id, set_id, event_type, event_metadata
) VALUES (
    $1, $2, $3, $4, $5, $6
);

-- name: SelectInvoiceEvents :many
SELECT *
FROM invoice_events
WHERE (
    invoice_id = sqlc.narg('invoice_id') OR 
    sqlc.narg('invoice_id') IS NULL
) AND (
    htlc_id = sqlc.narg('htlc_id') OR 
    sqlc.narg('htlc_id') IS NULL
) AND (
    set_id = sqlc.narg('set_id') OR 
    sqlc.narg('set_id') IS NULL
) AND (
    event_type = sqlc.narg('event_type') OR 
    sqlc.narg('event_type') IS NULL
) AND (
    created_at >= sqlc.narg('created_after') OR 
    sqlc.narg('created_after') IS NULL
) AND (
    created_at <= sqlc.narg('created_before') OR 
    sqlc.narg('created_before') IS NULL
) 
LIMIT @num_limit OFFSET @num_offset;

-- name: DeleteInvoiceEvents :exec
DELETE
FROM invoice_events
WHERE invoice_id = $1;
