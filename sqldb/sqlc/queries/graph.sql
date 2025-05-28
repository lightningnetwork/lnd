/* ─────────────────────────────────────────────
   nodes table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertNode :one
INSERT INTO nodes (
    version, pub_key, alias, last_update, color, signature
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (pub_key, version)
    -- Update the following fields if a conflict occurs on pub_key
    -- and version.
    DO UPDATE SET
        alias = EXCLUDED.alias,
        last_update = EXCLUDED.last_update,
        color = EXCLUDED.color,
        signature = EXCLUDED.signature
WHERE EXCLUDED.last_update > nodes.last_update
RETURNING id;

-- name: GetNodeByPubKey :one
SELECT *
FROM nodes
WHERE pub_key = $1
  AND version = $2;

-- name: DeleteNodeByPubKey :execresult
DELETE FROM nodes
WHERE pub_key = $1
  AND version = $2;

/* ─────────────────────────────────────────────
   node_features table queries
   ─────────────────────────────────────────────
*/

-- name: InsertNodeFeature :exec
INSERT INTO node_features (
    node_id, feature_bit
) VALUES (
    $1, $2
);

-- name: GetNodeFeatures :many
SELECT *
FROM node_features
WHERE node_id = $1;

-- name: GetNodeFeaturesByPubKey :many
SELECT f.feature_bit
FROM nodes n
    JOIN node_features f ON f.node_id = n.id
WHERE n.pub_key = $1
  AND n.version = $2;

-- name: DeleteNodeFeature :exec
DELETE FROM node_features
WHERE node_id = $1
  AND feature_bit = $2;

/* ─────────────────────────────────────────────
   node_addresses table queries
   ─────────────────────────────────────────────
*/

-- name: InsertNodeAddress :exec
INSERT INTO node_addresses (
    node_id,
    type,
    address,
    position
) VALUES (
    $1, $2, $3, $4
 );

-- name: GetNodeAddressesByPubKey :many
SELECT a.type, a.address
FROM nodes n
LEFT JOIN node_addresses a ON a.node_id = n.id
WHERE n.pub_key = $1 AND n.version = $2
ORDER BY a.type ASC, a.position ASC;

-- name: GetNodesByLastUpdateRange :many
SELECT *
FROM nodes
WHERE last_update >= @start_time
  AND last_update < @end_time;

-- name: DeleteNodeAddresses :exec
DELETE FROM node_addresses
WHERE node_id = $1;

/* ─────────────────────────────────────────────
   node_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertNodeExtraType :exec
INSERT INTO node_extra_types (
    node_id, type, value
)
VALUES ($1, $2, $3)
ON CONFLICT (type, node_id)
    -- Update the value if a conflict occurs on type
    -- and node_id.
    DO UPDATE SET value = EXCLUDED.value;

-- name: GetExtraNodeTypes :many
SELECT *
FROM node_extra_types
WHERE node_id = $1;

-- name: DeleteExtraNodeType :exec
DELETE FROM node_extra_types
WHERE node_id = $1
  AND type = $2;

/* ─────────────────────────────────────────────
   source_nodes table queries
   ─────────────────────────────────────────────
*/

-- name: AddSourceNode :exec
INSERT INTO source_nodes (node_id)
VALUES ($1)
ON CONFLICT (node_id) DO NOTHING;

-- name: GetSourceNodesByVersion :many
SELECT sn.node_id, n.pub_key
FROM source_nodes sn
    JOIN nodes n ON sn.node_id = n.id
WHERE n.version = $1;
