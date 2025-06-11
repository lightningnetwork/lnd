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
WHERE nodes.last_update IS NULL
    OR EXCLUDED.last_update > nodes.last_update
RETURNING id;

-- name: GetNodeByPubKey :one
SELECT *
FROM nodes
WHERE pub_key = $1
  AND version = $2;

-- name: GetNodeIDByPubKey :one
SELECT id
FROM nodes
WHERE pub_key = $1
  AND version = $2;

-- name: ListNodesPaginated :many
SELECT *
FROM nodes
WHERE version = $1 AND id > $2
ORDER BY id
LIMIT $3;

-- name: ListNodeIDsAndPubKeys :many
SELECT id, pub_key
FROM nodes
WHERE version = $1  AND id > $2
ORDER BY id
LIMIT $3;

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

/* ─────────────────────────────────────────────
   channels table queries
   ─────────────────────────────────────────────
*/

-- name: CreateChannel :one
INSERT INTO channels (
    version, scid, node_id_1, node_id_2,
    outpoint, capacity, bitcoin_key_1, bitcoin_key_2,
    node_1_signature, node_2_signature, bitcoin_1_signature,
    bitcoin_2_signature
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING id;

-- name: GetChannelBySCID :one
SELECT * FROM channels
WHERE scid = $1 AND version = $2;

-- name: GetChannelAndNodesBySCID :one
SELECT
    c.*,
    n1.pub_key AS node1_pub_key,
    n2.pub_key AS node2_pub_key
FROM channels c
    JOIN nodes n1 ON c.node_id_1 = n1.id
    JOIN nodes n2 ON c.node_id_2 = n2.id
WHERE c.scid = $1
  AND c.version = $2;

-- name: GetChannelFeaturesAndExtras :many
SELECT
    cf.channel_id,
    true AS is_feature,
    cf.feature_bit AS feature_bit,
    NULL AS extra_key,
    NULL AS value
FROM channel_features cf
WHERE cf.channel_id = $1

UNION ALL

SELECT
    cet.channel_id,
    false AS is_feature,
    0 AS feature_bit,
    cet.type AS extra_key,
    cet.value AS value
FROM channel_extra_types cet
WHERE cet.channel_id = $1;

-- name: GetChannelsByPolicyLastUpdateRange :many
SELECT
    sqlc.embed(c),
    sqlc.embed(n1),
    sqlc.embed(n2),

    -- Policy 1 (node_id_1)
    cp1.id AS policy1_id,
    cp1.node_id AS policy1_node_id,
    cp1.version AS policy1_version,
    cp1.timelock AS policy1_timelock,
    cp1.fee_ppm AS policy1_fee_ppm,
    cp1.base_fee_msat AS policy1_base_fee_msat,
    cp1.min_htlc_msat AS policy1_min_htlc_msat,
    cp1.max_htlc_msat AS policy1_max_htlc_msat,
    cp1.last_update AS policy1_last_update,
    cp1.disabled AS policy1_disabled,
    cp1.inbound_base_fee_msat AS policy1_inbound_base_fee_msat,
    cp1.inbound_fee_rate_milli_msat AS policy1_inbound_fee_rate_milli_msat,
    cp1.signature AS policy1_signature,

    -- Policy 2 (node_id_2)
    cp2.id AS policy2_id,
    cp2.node_id AS policy2_node_id,
    cp2.version AS policy2_version,
    cp2.timelock AS policy2_timelock,
    cp2.fee_ppm AS policy2_fee_ppm,
    cp2.base_fee_msat AS policy2_base_fee_msat,
    cp2.min_htlc_msat AS policy2_min_htlc_msat,
    cp2.max_htlc_msat AS policy2_max_htlc_msat,
    cp2.last_update AS policy2_last_update,
    cp2.disabled AS policy2_disabled,
    cp2.inbound_base_fee_msat AS policy2_inbound_base_fee_msat,
    cp2.inbound_fee_rate_milli_msat AS policy2_inbound_fee_rate_milli_msat,
    cp2.signature AS policy2_signature

FROM channels c
    JOIN nodes n1 ON c.node_id_1 = n1.id
    JOIN nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = @version
  AND (
       (cp1.last_update >= @start_time AND cp1.last_update < @end_time)
       OR
       (cp2.last_update >= @start_time AND cp2.last_update < @end_time)
  )
ORDER BY
    CASE
        WHEN COALESCE(cp1.last_update, 0) >= COALESCE(cp2.last_update, 0)
            THEN COALESCE(cp1.last_update, 0)
        ELSE COALESCE(cp2.last_update, 0)
        END ASC;

-- name: HighestSCID :one
SELECT scid
FROM channels
WHERE version = $1
ORDER BY scid DESC
LIMIT 1;

-- name: ListChannelsByNodeID :many
SELECT sqlc.embed(c),
    n1.pub_key AS node1_pubkey,
    n2.pub_key AS node2_pubkey,

    -- Policy 1
    -- TODO(elle): use sqlc.embed to embed policy structs
    --  once this issue is resolved:
    --  https://github.com/sqlc-dev/sqlc/issues/2997
    cp1.id AS policy1_id,
    cp1.node_id AS policy1_node_id,
    cp1.version AS policy1_version,
    cp1.timelock AS policy1_timelock,
    cp1.fee_ppm AS policy1_fee_ppm,
    cp1.base_fee_msat AS policy1_base_fee_msat,
    cp1.min_htlc_msat AS policy1_min_htlc_msat,
    cp1.max_htlc_msat AS policy1_max_htlc_msat,
    cp1.last_update AS policy1_last_update,
    cp1.disabled AS policy1_disabled,
    cp1.inbound_base_fee_msat AS policy1_inbound_base_fee_msat,
    cp1.inbound_fee_rate_milli_msat AS policy1_inbound_fee_rate_milli_msat,
    cp1.signature AS policy1_signature,

       -- Policy 2
    cp2.id AS policy2_id,
    cp2.node_id AS policy2_node_id,
    cp2.version AS policy2_version,
    cp2.timelock AS policy2_timelock,
    cp2.fee_ppm AS policy2_fee_ppm,
    cp2.base_fee_msat AS policy2_base_fee_msat,
    cp2.min_htlc_msat AS policy2_min_htlc_msat,
    cp2.max_htlc_msat AS policy2_max_htlc_msat,
    cp2.last_update AS policy2_last_update,
    cp2.disabled AS policy2_disabled,
    cp2.inbound_base_fee_msat AS policy2_inbound_base_fee_msat,
    cp2.inbound_fee_rate_milli_msat AS policy2_inbound_fee_rate_milli_msat,
    cp2.signature AS policy2_signature

FROM channels c
    JOIN nodes n1 ON c.node_id_1 = n1.id
    JOIN nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN channel_policies cp1
    ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN channel_policies cp2
    ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1
  AND (c.node_id_1 = $2 OR c.node_id_2 = $2);

-- name: GetPublicV1ChannelsBySCID :many
SELECT *
FROM channels
WHERE node_1_signature IS NOT NULL
  AND scid >= @start_scid
  AND scid < @end_scid;

-- name: ListChannelsWithPoliciesPaginated :many
SELECT
    sqlc.embed(c),

    -- Join node pubkeys
    n1.pub_key AS node1_pubkey,
    n2.pub_key AS node2_pubkey,

    -- Node 1 policy
    cp1.id AS policy_1_id,
    cp1.node_id AS policy_1_node_id,
    cp1.version AS policy_1_version,
    cp1.timelock AS policy_1_timelock,
    cp1.fee_ppm AS policy_1_fee_ppm,
    cp1.base_fee_msat AS policy_1_base_fee_msat,
    cp1.min_htlc_msat AS policy_1_min_htlc_msat,
    cp1.max_htlc_msat AS policy_1_max_htlc_msat,
    cp1.last_update AS policy_1_last_update,
    cp1.disabled AS policy_1_disabled,
    cp1.inbound_base_fee_msat AS policy1_inbound_base_fee_msat,
    cp1.inbound_fee_rate_milli_msat AS policy1_inbound_fee_rate_milli_msat,
    cp1.signature AS policy_1_signature,

    -- Node 2 policy
    cp2.id AS policy_2_id,
    cp2.node_id AS policy_2_node_id,
    cp2.version AS policy_2_version,
    cp2.timelock AS policy_2_timelock,
    cp2.fee_ppm AS policy_2_fee_ppm,
    cp2.base_fee_msat AS policy_2_base_fee_msat,
    cp2.min_htlc_msat AS policy_2_min_htlc_msat,
    cp2.max_htlc_msat AS policy_2_max_htlc_msat,
    cp2.last_update AS policy_2_last_update,
    cp2.disabled AS policy_2_disabled,
    cp2.inbound_base_fee_msat AS policy2_inbound_base_fee_msat,
    cp2.inbound_fee_rate_milli_msat AS policy2_inbound_fee_rate_milli_msat,
    cp2.signature AS policy_2_signature

FROM channels c
JOIN nodes n1 ON c.node_id_1 = n1.id
JOIN nodes n2 ON c.node_id_2 = n2.id
LEFT JOIN channel_policies cp1
    ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
LEFT JOIN channel_policies cp2
    ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1 AND c.id > $2
ORDER BY c.id
LIMIT $3;

/* ─────────────────────────────────────────────
   channel_features table queries
   ─────────────────────────────────────────────
*/

-- name: InsertChannelFeature :exec
INSERT INTO channel_features (
    channel_id, feature_bit
) VALUES (
    $1, $2
);

/* ─────────────────────────────────────────────
   channel_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: CreateChannelExtraType :exec
INSERT INTO channel_extra_types (
    channel_id, type, value
)
VALUES ($1, $2, $3);

/* ─────────────────────────────────────────────
   channel_policies table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertEdgePolicy :one
INSERT INTO channel_policies (
    version, channel_id, node_id, timelock, fee_ppm,
    base_fee_msat, min_htlc_msat, last_update, disabled,
    max_htlc_msat, inbound_base_fee_msat,
    inbound_fee_rate_milli_msat, signature
) VALUES  (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
)
ON CONFLICT (channel_id, node_id, version)
    -- Update the following fields if a conflict occurs on channel_id,
    -- node_id, and version.
    DO UPDATE SET
        timelock = EXCLUDED.timelock,
        fee_ppm = EXCLUDED.fee_ppm,
        base_fee_msat = EXCLUDED.base_fee_msat,
        min_htlc_msat = EXCLUDED.min_htlc_msat,
        last_update = EXCLUDED.last_update,
        disabled = EXCLUDED.disabled,
        max_htlc_msat = EXCLUDED.max_htlc_msat,
        inbound_base_fee_msat = EXCLUDED.inbound_base_fee_msat,
        inbound_fee_rate_milli_msat = EXCLUDED.inbound_fee_rate_milli_msat,
        signature = EXCLUDED.signature
WHERE EXCLUDED.last_update > channel_policies.last_update
RETURNING id;

-- name: GetChannelPolicyByChannelAndNode :one
SELECT *
FROM channel_policies
WHERE channel_id = $1
  AND node_id = $2
  AND version = $3;

/* ─────────────────────────────────────────────
   channel_policy_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: InsertChanPolicyExtraType :exec
INSERT INTO channel_policy_extra_types (
    channel_policy_id, type, value
)
VALUES ($1, $2, $3);

-- name: GetChannelPolicyExtraTypes :many
SELECT
    cp.id AS policy_id,
    cp.channel_id,
    cp.node_id,
    cpet.type,
    cpet.value
FROM channel_policies cp
JOIN channel_policy_extra_types cpet
ON cp.id = cpet.channel_policy_id
WHERE cp.id = $1 OR cp.id = $2;

-- name: DeleteChannelPolicyExtraTypes :exec
DELETE FROM channel_policy_extra_types
WHERE channel_policy_id = $1;
