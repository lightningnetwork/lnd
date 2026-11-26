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

-- name: DeleteOpenBolt12Invoice :execresult
DELETE
FROM invoices
WHERE invoices.id = $1
    AND state = 0
    AND EXISTS (
        SELECT 1
        FROM bolt12_invoices
        WHERE bolt12_invoices.invoice_id = invoices.id
            AND bolt12_invoices.offer_id IS NOT NULL
    )
    AND NOT EXISTS (
        SELECT 1
        FROM invoice_htlcs
        WHERE invoice_htlcs.invoice_id = invoices.id
            AND invoice_htlcs.state = 0
    );
