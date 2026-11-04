-- name: InsertBolt12Invoice :exec
INSERT INTO bolt12_invoices (
    invoice_id, offer_id, envelope
) VALUES (
    $1, $2, $3
);

-- name: FetchBolt12Invoice :one
SELECT offer_id, envelope
FROM bolt12_invoices
WHERE invoice_id = $1;
