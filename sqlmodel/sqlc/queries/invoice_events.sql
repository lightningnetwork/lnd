-- name: OnInvoiceCreated :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id
) VALUES (
    $1, 0, $2
);

-- name: OnInvoiceCanceled :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id
) VALUES (
    $1, 1, $2
);

-- name: OnInvoiceSettled :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id
) VALUES (
    $1, 2, $2
);

-- name: OnAMPSubInvoiceCreated :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id, set_id
) VALUES (
    $1, 3, $2, $3
);

-- name: OnAMPSubInvoiceCanceled :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id, set_id
) VALUES (
    $1, 4, $2, $3
);

-- name: OnAMPSubInvoiceSettled :exec
INSERT INTO invoice_events (
    added_at, event_type, invoice_id, set_id
) VALUES (
    $1, 5, $2, $3
);
