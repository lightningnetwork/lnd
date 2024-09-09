-- name: InsertBlindedPath :one
INSERT INTO blinded_paths (
    invoice_id, last_ephemeral_pub, session_key, introduction_node,
    amount_msat
) VALUES (
    $1, $2, $3, $4, $5
) RETURNING id;

-- name: FetchBlindedPaths :many
SELECT *
FROM blinded_paths
WHERE invoice_id = $1;

-- name: InsertBlindedPathHop :exec
INSERT INTO blinded_path_hops (
    blinded_path_id, hop_index, channel_id, node_pub_key,
    amount_to_fwd
) VALUES (
    $1, $2, $3, $4, $5
);

-- name: FetchBlindedPathHops :many
SELECT *
FROM blinded_path_hops
WHERE blinded_path_id = $1
ORDER BY hop_index;
