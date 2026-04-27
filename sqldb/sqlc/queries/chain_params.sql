-- name: InsertChainNetwork :exec
INSERT INTO chain_params (single_row, network)
VALUES (TRUE, sqlc.arg(network))
ON CONFLICT (single_row) DO NOTHING;

-- name: GetChainNetwork :one
SELECT network FROM chain_params
WHERE single_row = TRUE;
