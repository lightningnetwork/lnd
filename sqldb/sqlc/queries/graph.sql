/* ─────────────────────────────────────────────
   graph_nodes table queries
   ───────────────────────────��─────────────────
*/

-- name: UpsertNode :one
INSERT INTO graph_nodes (
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
WHERE graph_nodes.last_update IS NULL
    OR EXCLUDED.last_update > graph_nodes.last_update
RETURNING id;

-- We use a separate upsert for our own node since we want to be less strict
-- about the last_update field. For our own node, we always want to
-- update the record even if the last_update is the same as what we have.
-- name: UpsertSourceNode :one
INSERT INTO graph_nodes (
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
WHERE graph_nodes.last_update IS NULL
    OR EXCLUDED.last_update >= graph_nodes.last_update
RETURNING id;

-- name: GetNodesByIDs :many
SELECT *
FROM graph_nodes
WHERE id IN (sqlc.slice('ids')/*SLICE:ids*/);

-- name: GetNodeByPubKey :one
SELECT *
FROM graph_nodes
WHERE pub_key = $1
  AND version = $2;

-- name: GetNodeIDByPubKey :one
SELECT id
FROM graph_nodes
WHERE pub_key = $1
  AND version = $2;

-- name: ListNodesPaginated :many
SELECT *
FROM graph_nodes
WHERE version = $1 AND id > $2
ORDER BY id
LIMIT $3;

-- name: ListNodeIDsAndPubKeys :many
SELECT id, pub_key
FROM graph_nodes
WHERE version = $1  AND id > $2
ORDER BY id
LIMIT $3;

-- name: IsPublicV1Node :one
SELECT EXISTS (
    SELECT 1
    FROM graph_channels c
    JOIN graph_nodes n ON n.id = c.node_id_1 OR n.id = c.node_id_2
    -- NOTE: we hard-code the version here since the clauses
    -- here that determine if a node is public is specific
    -- to the V1 gossip protocol. In V1, a node is public
    -- if it has a public channel and a public channel is one
    -- where we have the set of signatures of the channel
    -- announcement. It is enough to just check that we have
    -- one of the signatures since we only ever set them
    -- together.
    WHERE c.version = 1
      AND c.bitcoin_1_signature IS NOT NULL
      AND n.pub_key = $1
);

-- name: DeleteUnconnectedNodes :many
DELETE FROM graph_nodes
WHERE
    -- Ignore any of our source nodes.
    NOT EXISTS (
        SELECT 1
        FROM graph_source_nodes sn
        WHERE sn.node_id = graph_nodes.id
    )
    -- Select all nodes that do not have any channels.
    AND NOT EXISTS (
        SELECT 1
        FROM graph_channels c
        WHERE c.node_id_1 = graph_nodes.id OR c.node_id_2 = graph_nodes.id
) RETURNING pub_key;

-- name: DeleteNodeByPubKey :execresult
DELETE FROM graph_nodes
WHERE pub_key = $1
  AND version = $2;

-- name: DeleteNode :exec
DELETE FROM graph_nodes
WHERE id = $1;

/* ─────────────────────────────────────────────
   graph_node_features table queries
   ─────────────────────────────────────────────
*/

-- name: InsertNodeFeature :exec
INSERT INTO graph_node_features (
    node_id, feature_bit
) VALUES (
    $1, $2
) ON CONFLICT (node_id, feature_bit)
    -- Do nothing if the feature already exists for the node.
    DO NOTHING;

-- name: GetNodeFeatures :many
SELECT *
FROM graph_node_features
WHERE node_id = $1;

-- name: GetNodeFeaturesByPubKey :many
SELECT f.feature_bit
FROM graph_nodes n
    JOIN graph_node_features f ON f.node_id = n.id
WHERE n.pub_key = $1
  AND n.version = $2;

-- name: GetNodeFeaturesBatch :many
SELECT node_id, feature_bit
FROM graph_node_features
WHERE node_id IN (sqlc.slice('ids')/*SLICE:ids*/)
ORDER BY node_id, feature_bit;

-- name: DeleteNodeFeature :exec
DELETE FROM graph_node_features
WHERE node_id = $1
  AND feature_bit = $2;

/* ─────────────────────────────────────────────
   graph_node_addresses table queries
   ───────────────────────────────────��─────────
*/

-- name: UpsertNodeAddress :exec
INSERT INTO graph_node_addresses (
    node_id,
    type,
    address,
    position
) VALUES (
    $1, $2, $3, $4
) ON CONFLICT (node_id, type, position)
    DO UPDATE SET address = EXCLUDED.address;

-- name: GetNodeAddresses :many
SELECT type, address
FROM graph_node_addresses
WHERE node_id = $1
ORDER BY type ASC, position ASC;

-- name: GetNodeAddressesBatch :many
SELECT node_id, type, position, address
FROM graph_node_addresses
WHERE node_id IN (sqlc.slice('ids')/*SLICE:ids*/)
ORDER BY node_id, type, position;

-- name: GetNodesByLastUpdateRange :many
SELECT *
FROM graph_nodes
WHERE last_update >= @start_time
  AND last_update <= @end_time
  -- Pagination: We use (last_update, pub_key) as a compound cursor.
  -- This ensures stable ordering and allows us to resume from where we left off.
  -- We use COALESCE with -1 as sentinel since timestamps are always positive.
  AND (
    -- Include rows with last_update greater than cursor (or all rows if cursor is -1)
    last_update > COALESCE(sqlc.narg('last_update'), -1)
    OR 
    -- For rows with same last_update, use pub_key as tiebreaker
    (last_update = COALESCE(sqlc.narg('last_update'), -1) 
     AND pub_key > sqlc.narg('last_pub_key'))
  )
  -- Optional filter for public nodes only
  AND (
    -- If only_public is false or not provided, include all nodes
    COALESCE(sqlc.narg('only_public'), FALSE) IS FALSE
    OR 
    -- For V1 protocol, a node is public if it has at least one public channel.
    -- A public channel has bitcoin_1_signature set (channel announcement received).
    EXISTS (
      SELECT 1
      FROM graph_channels c
      WHERE c.version = 1
        AND c.bitcoin_1_signature IS NOT NULL
        AND (c.node_id_1 = graph_nodes.id OR c.node_id_2 = graph_nodes.id)
    )
  )
ORDER BY last_update ASC, pub_key ASC
LIMIT COALESCE(sqlc.narg('max_results'), 999999999);

-- name: DeleteNodeAddresses :exec
DELETE FROM graph_node_addresses
WHERE node_id = $1;

/* ─────────────────────────────────────────────
   graph_node_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertNodeExtraType :exec
INSERT INTO graph_node_extra_types (
    node_id, type, value
)
VALUES ($1, $2, $3)
ON CONFLICT (type, node_id)
    -- Update the value if a conflict occurs on type
    -- and node_id.
    DO UPDATE SET value = EXCLUDED.value;

-- name: GetExtraNodeTypes :many
SELECT *
FROM graph_node_extra_types
WHERE node_id = $1;

-- name: GetNodeExtraTypesBatch :many
SELECT node_id, type, value
FROM graph_node_extra_types
WHERE node_id IN (sqlc.slice('ids')/*SLICE:ids*/)
ORDER BY node_id, type;

-- name: DeleteExtraNodeType :exec
DELETE FROM graph_node_extra_types
WHERE node_id = $1
  AND type = $2;

/* ─────────────────────────────────────────────
   graph_source_nodes table queries
   ─────────────────────────────────────────────
*/

-- name: AddSourceNode :exec
INSERT INTO graph_source_nodes (node_id)
VALUES ($1)
ON CONFLICT (node_id) DO NOTHING;

-- name: GetSourceNodesByVersion :many
SELECT sn.node_id, n.pub_key
FROM graph_source_nodes sn
    JOIN graph_nodes n ON sn.node_id = n.id
WHERE n.version = $1;

/* ─────────────────────────────────────────────
   graph_channels table queries
   ─────────────────────────────────────────────
*/

-- name: CreateChannel :one
INSERT INTO graph_channels (
    version, scid, node_id_1, node_id_2,
    outpoint, capacity, bitcoin_key_1, bitcoin_key_2,
    node_1_signature, node_2_signature, bitcoin_1_signature,
    bitcoin_2_signature
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
)
RETURNING id;

-- name: AddV1ChannelProof :execresult
UPDATE graph_channels
SET node_1_signature = $2,
    node_2_signature = $3,
    bitcoin_1_signature = $4,
    bitcoin_2_signature = $5
WHERE scid = $1
  AND version = 1;

-- name: GetChannelsBySCIDRange :many
SELECT sqlc.embed(c),
    n1.pub_key AS node1_pub_key,
    n2.pub_key AS node2_pub_key
FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
WHERE scid >= @start_scid
  AND scid < @end_scid;

-- name: GetChannelBySCID :one
SELECT * FROM graph_channels
WHERE scid = $1 AND version = $2;

-- name: GetChannelsBySCIDs :many
SELECT * FROM graph_channels
WHERE version = @version
  AND scid IN (sqlc.slice('scids')/*SLICE:scids*/);

-- name: GetChannelsByOutpoints :many
SELECT
    sqlc.embed(c),
    n1.pub_key AS node1_pubkey,
    n2.pub_key AS node2_pubkey
FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
WHERE c.outpoint IN
    (sqlc.slice('outpoints')/*SLICE:outpoints*/);

-- name: GetChannelAndNodesBySCID :one
SELECT
    c.*,
    n1.pub_key AS node1_pub_key,
    n2.pub_key AS node2_pub_key
FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
WHERE c.scid = $1
  AND c.version = $2;

-- name: GetSCIDByOutpoint :one
SELECT scid from graph_channels
WHERE outpoint = $1 AND version = $2;

-- name: GetChannelsBySCIDWithPolicies :many
SELECT
    sqlc.embed(c),
    sqlc.embed(n1),
    sqlc.embed(n2),

    -- Policy 1
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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy_2_message_flags,
    cp2.channel_flags AS policy_2_channel_flags,
    cp2.signature AS policy2_signature

FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE
    c.version = @version
  AND c.scid IN (sqlc.slice('scids')/*SLICE:scids*/);

-- name: GetChannelsByIDs :many
SELECT
    sqlc.embed(c),

    -- Minimal node data.
    n1.id AS node1_id,
    n1.pub_key AS node1_pub_key,
    n2.id AS node2_id,
    n2.pub_key AS node2_pub_key,

    -- Policy 1
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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy2_message_flags,
    cp2.channel_flags AS policy2_channel_flags,
    cp2.signature AS policy2_signature

FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.id IN (sqlc.slice('ids')/*SLICE:ids*/);

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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy2_message_flags,
    cp2.channel_flags AS policy2_channel_flags,
    cp2.signature AS policy2_signature

FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = @version
  AND (
       (cp1.last_update >= @start_time AND cp1.last_update < @end_time)
       OR
       (cp2.last_update >= @start_time AND cp2.last_update < @end_time)
  )
  -- Pagination using compound cursor (max_update_time, id).
  -- We use COALESCE with -1 as sentinel since timestamps are always positive.
  AND (
       (CASE
           WHEN COALESCE(cp1.last_update, 0) >= COALESCE(cp2.last_update, 0)
               THEN COALESCE(cp1.last_update, 0)
           ELSE COALESCE(cp2.last_update, 0)
       END > COALESCE(sqlc.narg('last_update_time'), -1))
       OR 
       (CASE
           WHEN COALESCE(cp1.last_update, 0) >= COALESCE(cp2.last_update, 0)
               THEN COALESCE(cp1.last_update, 0)
           ELSE COALESCE(cp2.last_update, 0)
       END = COALESCE(sqlc.narg('last_update_time'), -1) 
       AND c.id > COALESCE(sqlc.narg('last_id'), -1))
  )
ORDER BY
    CASE
        WHEN COALESCE(cp1.last_update, 0) >= COALESCE(cp2.last_update, 0)
            THEN COALESCE(cp1.last_update, 0)
        ELSE COALESCE(cp2.last_update, 0)
    END ASC,
    c.id ASC
LIMIT COALESCE(sqlc.narg('max_results'), 999999999);

-- name: GetChannelByOutpointWithPolicies :one
SELECT
    sqlc.embed(c),

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
    cp1.message_flags AS policy_1_message_flags,
    cp1.channel_flags AS policy_1_channel_flags,
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
    cp2.message_flags AS policy_2_message_flags,
    cp2.channel_flags AS policy_2_channel_flags,
    cp2.signature AS policy_2_signature
FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.outpoint = $1 AND c.version = $2;

-- name: HighestSCID :one
SELECT scid
FROM graph_channels
WHERE version = $1
ORDER BY scid DESC
LIMIT 1;

-- name: ListChannelsForNodeIDs :many
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
       cp1.message_flags AS policy1_message_flags,
       cp1.channel_flags AS policy1_channel_flags,
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
       cp2.message_flags AS policy2_message_flags,
       cp2.channel_flags AS policy2_channel_flags,
       cp2.signature AS policy2_signature

FROM graph_channels c
         JOIN graph_nodes n1 ON c.node_id_1 = n1.id
         JOIN graph_nodes n2 ON c.node_id_2 = n2.id
         LEFT JOIN graph_channel_policies cp1
                   ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
         LEFT JOIN graph_channel_policies cp2
                   ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1
  AND (c.node_id_1 IN (sqlc.slice('node1_ids')/*SLICE:node1_ids*/)
   OR c.node_id_2 IN (sqlc.slice('node2_ids')/*SLICE:node2_ids*/));

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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy2_message_flags,
    cp2.channel_flags AS policy2_channel_flags,
    cp2.signature AS policy2_signature

FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
    ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
    ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1
  AND (c.node_id_1 = $2 OR c.node_id_2 = $2);

-- name: GetPublicV1ChannelsBySCID :many
SELECT *
FROM graph_channels
WHERE node_1_signature IS NOT NULL
  AND scid >= @start_scid
  AND scid < @end_scid;

-- name: ListChannelsPaginated :many
SELECT id, bitcoin_key_1, bitcoin_key_2, outpoint
FROM graph_channels c
WHERE c.version = $1 AND c.id > $2
ORDER BY c.id
LIMIT $3;

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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy2_message_flags,
    cp2.channel_flags AS policy2_channel_flags,
    cp2.signature AS policy_2_signature

FROM graph_channels c
JOIN graph_nodes n1 ON c.node_id_1 = n1.id
JOIN graph_nodes n2 ON c.node_id_2 = n2.id
LEFT JOIN graph_channel_policies cp1
    ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
LEFT JOIN graph_channel_policies cp2
    ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1 AND c.id > $2
ORDER BY c.id
LIMIT $3;

-- name: ListChannelsWithPoliciesForCachePaginated :many
SELECT
    c.id as id,
    c.scid as scid,
    c.capacity AS capacity,

    -- Join node pubkeys
    n1.pub_key AS node1_pubkey,
    n2.pub_key AS node2_pubkey,

    -- Node 1 policy
    cp1.timelock AS policy_1_timelock,
    cp1.fee_ppm AS policy_1_fee_ppm,
    cp1.base_fee_msat AS policy_1_base_fee_msat,
    cp1.min_htlc_msat AS policy_1_min_htlc_msat,
    cp1.max_htlc_msat AS policy_1_max_htlc_msat,
    cp1.disabled AS policy_1_disabled,
    cp1.inbound_base_fee_msat AS policy1_inbound_base_fee_msat,
    cp1.inbound_fee_rate_milli_msat AS policy1_inbound_fee_rate_milli_msat,
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,

    -- Node 2 policy
    cp2.timelock AS policy_2_timelock,
    cp2.fee_ppm AS policy_2_fee_ppm,
    cp2.base_fee_msat AS policy_2_base_fee_msat,
    cp2.min_htlc_msat AS policy_2_min_htlc_msat,
    cp2.max_htlc_msat AS policy_2_max_htlc_msat,
    cp2.disabled AS policy_2_disabled,
    cp2.inbound_base_fee_msat AS policy2_inbound_base_fee_msat,
    cp2.inbound_fee_rate_milli_msat AS policy2_inbound_fee_rate_milli_msat,
    cp2.message_flags AS policy2_message_flags,
    cp2.channel_flags AS policy2_channel_flags

FROM graph_channels c
JOIN graph_nodes n1 ON c.node_id_1 = n1.id
JOIN graph_nodes n2 ON c.node_id_2 = n2.id
LEFT JOIN graph_channel_policies cp1
    ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
LEFT JOIN graph_channel_policies cp2
    ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.version = $1 AND c.id > $2
ORDER BY c.id
LIMIT $3;

-- name: DeleteChannels :exec
DELETE FROM graph_channels
WHERE id IN (sqlc.slice('ids')/*SLICE:ids*/);

/* ─────────────────────────────────────────────
   graph_channel_features table queries
   ─────────────────────────────────────────────
*/

-- name: InsertChannelFeature :exec
INSERT INTO graph_channel_features (
    channel_id, feature_bit
) VALUES (
    $1, $2
) ON CONFLICT (channel_id, feature_bit)
    -- Do nothing if the channel_id and feature_bit already exist.
    DO NOTHING;

-- name: GetChannelFeaturesBatch :many
SELECT
    channel_id,
    feature_bit
FROM graph_channel_features
WHERE channel_id IN (sqlc.slice('chan_ids')/*SLICE:chan_ids*/)
ORDER BY channel_id, feature_bit;

/* ─────────────────────────────────────────────
   graph_channel_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertChannelExtraType :exec
INSERT INTO graph_channel_extra_types (
    channel_id, type, value
)
VALUES ($1, $2, $3)
    ON CONFLICT (channel_id, type)
    -- Update the value if a conflict occurs on channel_id and type.
    DO UPDATE SET value = EXCLUDED.value;

-- name: GetChannelExtrasBatch :many
SELECT
    channel_id,
    type,
    value
FROM graph_channel_extra_types
WHERE channel_id IN (sqlc.slice('chan_ids')/*SLICE:chan_ids*/)
ORDER BY channel_id, type;

/* ─────────────────────────────────────────────
   graph_channel_policies table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertEdgePolicy :one
INSERT INTO graph_channel_policies (
    version, channel_id, node_id, timelock, fee_ppm,
    base_fee_msat, min_htlc_msat, last_update, disabled,
    max_htlc_msat, inbound_base_fee_msat,
    inbound_fee_rate_milli_msat, message_flags, channel_flags,
    signature
) VALUES  (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
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
        message_flags = EXCLUDED.message_flags,
        channel_flags = EXCLUDED.channel_flags,
        signature = EXCLUDED.signature
WHERE EXCLUDED.last_update > graph_channel_policies.last_update
RETURNING id;

-- name: GetChannelPolicyByChannelAndNode :one
SELECT *
FROM graph_channel_policies
WHERE channel_id = $1
  AND node_id = $2
  AND version = $3;

-- name: GetChannelBySCIDWithPolicies :one
SELECT
    sqlc.embed(c),
    sqlc.embed(n1),
    sqlc.embed(n2),

    -- Policy 1
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
    cp1.message_flags AS policy1_message_flags,
    cp1.channel_flags AS policy1_channel_flags,
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
    cp2.message_flags AS policy_2_message_flags,
    cp2.channel_flags AS policy_2_channel_flags,
    cp2.signature AS policy2_signature

FROM graph_channels c
    JOIN graph_nodes n1 ON c.node_id_1 = n1.id
    JOIN graph_nodes n2 ON c.node_id_2 = n2.id
    LEFT JOIN graph_channel_policies cp1
        ON cp1.channel_id = c.id AND cp1.node_id = c.node_id_1 AND cp1.version = c.version
    LEFT JOIN graph_channel_policies cp2
        ON cp2.channel_id = c.id AND cp2.node_id = c.node_id_2 AND cp2.version = c.version
WHERE c.scid = @scid
  AND c.version = @version;

/* ─────────────────────────────────────────────
   graph_channel_policy_extra_types table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertChanPolicyExtraType :exec
INSERT INTO graph_channel_policy_extra_types (
    channel_policy_id, type, value
)
VALUES ($1, $2, $3)
ON CONFLICT (channel_policy_id, type)
    -- If a conflict occurs on channel_policy_id and type, then we update the
    -- value.
    DO UPDATE SET value = EXCLUDED.value;

-- name: GetChannelPolicyExtraTypesBatch :many
SELECT
    channel_policy_id as policy_id,
    type,
    value
FROM graph_channel_policy_extra_types
WHERE channel_policy_id IN (sqlc.slice('policy_ids')/*SLICE:policy_ids*/)
ORDER BY channel_policy_id, type;

-- name: GetV1DisabledSCIDs :many
SELECT c.scid
FROM graph_channels c
    JOIN graph_channel_policies cp ON cp.channel_id = c.id
-- NOTE: this is V1 specific since for V1, disabled is a
-- simple, single boolean. The proposed V2 policy
-- structure will have a more complex disabled bit vector
-- and so the query for V2 may differ.
WHERE cp.disabled = true
AND c.version = 1
GROUP BY c.scid
HAVING COUNT(*) > 1;

-- name: DeleteChannelPolicyExtraTypes :exec
DELETE FROM graph_channel_policy_extra_types
WHERE channel_policy_id = $1;

/* ─────────────────────────────────────────────
   graph_zombie_channels table queries
   ─────────────────────────────────────────────
*/

-- name: UpsertZombieChannel :exec
INSERT INTO graph_zombie_channels (scid, version, node_key_1, node_key_2)
VALUES ($1, $2, $3, $4)
ON CONFLICT (scid, version)
DO UPDATE SET
    -- If a conflict exists for the SCID and version pair, then we
    -- update the node keys.
    node_key_1 = COALESCE(EXCLUDED.node_key_1, graph_zombie_channels.node_key_1),
    node_key_2 = COALESCE(EXCLUDED.node_key_2, graph_zombie_channels.node_key_2);

-- name: GetZombieChannelsSCIDs :many
SELECT *
FROM graph_zombie_channels
WHERE version = @version
  AND scid IN (sqlc.slice('scids')/*SLICE:scids*/);

-- name: DeleteZombieChannel :execresult
DELETE FROM graph_zombie_channels
WHERE scid = $1
AND version = $2;

-- name: CountZombieChannels :one
SELECT COUNT(*)
FROM graph_zombie_channels
WHERE version = $1;

-- name: GetZombieChannel :one
SELECT *
FROM graph_zombie_channels
WHERE scid = $1
AND version = $2;

-- name: IsZombieChannel :one
SELECT EXISTS (
    SELECT 1
    FROM graph_zombie_channels
    WHERE scid = $1
    AND version = $2
) AS is_zombie;

/* ───────────────────────────���─────────────────
    graph_prune_log table queries
    ─────────────────────────────────────────────
*/

-- name: UpsertPruneLogEntry :exec
INSERT INTO graph_prune_log (
    block_height, block_hash
) VALUES (
    $1, $2
)
ON CONFLICT(block_height) DO UPDATE SET
    block_hash = EXCLUDED.block_hash;

-- name: GetPruneTip :one
SELECT block_height, block_hash
FROM graph_prune_log
ORDER BY block_height DESC
LIMIT 1;

-- name: GetPruneEntriesForHeights :many
SELECT block_height, block_hash
FROM graph_prune_log
WHERE block_height
   IN (sqlc.slice('heights')/*SLICE:heights*/);

-- name: GetPruneHashByHeight :one
SELECT block_hash
FROM graph_prune_log
WHERE block_height = $1;

-- name: DeletePruneLogEntriesInRange :exec
DELETE FROM graph_prune_log
WHERE block_height >= @start_height
  AND block_height <= @end_height;

/* ─────────────────────────────────────────────
   graph_closed_scid table queries
   ────────────────────────────────────────────-
*/

-- name: InsertClosedChannel :exec
INSERT INTO graph_closed_scids (scid)
VALUES ($1)
ON CONFLICT (scid) DO NOTHING;

-- name: IsClosedChannel :one
SELECT EXISTS (
    SELECT 1
    FROM graph_closed_scids
    WHERE scid = $1
);

-- name: GetClosedChannelsSCIDs :many
SELECT scid
FROM graph_closed_scids
WHERE scid IN (sqlc.slice('scids')/*SLICE:scids*/);

/* ─────────────────────────────────────────────
   Migration specific queries

   NOTE: once sqldbv2 is in place, these queries can be contained to a package
   dedicated to the migration that requires it, and so we can then remove
   it from the main set of "live" queries that the code-base has access to.
   ────────────────────────────────────────────-
*/

-- NOTE: This query is only meant to be used by the graph SQL migration since
-- for that migration, in order to be retry-safe, we don't want to error out if
-- we re-insert the same node (which would error if the normal UpsertNode query
-- is used because of the constraint in that query that requires a node update
-- to have a newer last_update than the existing node).
-- name: InsertNodeMig :one
INSERT INTO graph_nodes (
    version, pub_key, alias, last_update, color, signature
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (pub_key, version)
    -- If a conflict occurs, we have already migrated this node. However, we
    -- still need to do an "UPDATE SET" here instead of "DO NOTHING" because
    -- otherwise, the "RETURNING id" part does not work.
    DO UPDATE SET
        alias = EXCLUDED.alias,
        last_update = EXCLUDED.last_update,
        color = EXCLUDED.color,
        signature = EXCLUDED.signature
RETURNING id;

-- NOTE: This query is only meant to be used by the graph SQL migration since
-- for that migration, in order to be retry-safe, we don't want to error out if
-- we re-insert the same channel again (which would error if the normal
-- CreateChannel query is used because of the uniqueness constraint on the scid
-- and version columns).
-- name: InsertChannelMig :one
INSERT INTO graph_channels (
    version, scid, node_id_1, node_id_2,
    outpoint, capacity, bitcoin_key_1, bitcoin_key_2,
    node_1_signature, node_2_signature, bitcoin_1_signature,
    bitcoin_2_signature
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12
) ON CONFLICT (scid, version)
    -- If a conflict occurs, we have already migrated this channel. However, we
    -- still need to do an "UPDATE SET" here instead of "DO NOTHING" because
    -- otherwise, the "RETURNING id" part does not work.
    DO UPDATE SET
        node_id_1 = EXCLUDED.node_id_1,
        node_id_2 = EXCLUDED.node_id_2,
        outpoint = EXCLUDED.outpoint,
        capacity = EXCLUDED.capacity,
        bitcoin_key_1 = EXCLUDED.bitcoin_key_1,
        bitcoin_key_2 = EXCLUDED.bitcoin_key_2,
        node_1_signature = EXCLUDED.node_1_signature,
        node_2_signature = EXCLUDED.node_2_signature,
        bitcoin_1_signature = EXCLUDED.bitcoin_1_signature,
        bitcoin_2_signature = EXCLUDED.bitcoin_2_signature
RETURNING id;

-- NOTE: This query is only meant to be used by the graph SQL migration since
-- for that migration, in order to be retry-safe, we don't want to error out if
-- we re-insert the same policy (which would error if the normal
-- UpsertEdgePolicy query is used because of the constraint in that query that
-- requires a policy update to have a newer last_update than the existing one).
-- name: InsertEdgePolicyMig :one
INSERT INTO graph_channel_policies (
    version, channel_id, node_id, timelock, fee_ppm,
    base_fee_msat, min_htlc_msat, last_update, disabled,
    max_htlc_msat, inbound_base_fee_msat,
    inbound_fee_rate_milli_msat, message_flags, channel_flags,
    signature
) VALUES  (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
)
ON CONFLICT (channel_id, node_id, version)
    -- If a conflict occurs, we have already migrated this policy. However, we
    -- still need to do an "UPDATE SET" here instead of "DO NOTHING" because
    -- otherwise, the "RETURNING id" part does not work.
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
        message_flags = EXCLUDED.message_flags,
        channel_flags = EXCLUDED.channel_flags,
        signature = EXCLUDED.signature
RETURNING id;