-- Preferred-node mapping: one row per unique pub_key pointing at the "best"
-- node row across gossip versions.  Priority: v2 announced > v1 announced >
-- v2 shell > v1 shell.
CREATE TABLE IF NOT EXISTS graph_preferred_nodes (
    pub_key  BLOB PRIMARY KEY,
    node_id  BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE
);

-- Index on node_id so cascade deletes from graph_nodes can locate the
-- referencing rows without a sequential scan.
CREATE INDEX IF NOT EXISTS graph_preferred_nodes_node_id_idx
    ON graph_preferred_nodes (node_id);

-- Preferred-channel mapping: one row per unique SCID pointing at the "best"
-- channel row across gossip versions.  Priority: v2 with policies >
-- v1 with policies > v2 bare > v1 bare.
CREATE TABLE IF NOT EXISTS graph_preferred_channels (
    scid       BLOB PRIMARY KEY,
    channel_id BIGINT NOT NULL REFERENCES graph_channels(id) ON DELETE CASCADE
);

-- Index on channel_id so cascade deletes from graph_channels can locate
-- the referencing rows without a sequential scan.
CREATE INDEX IF NOT EXISTS graph_preferred_channels_channel_id_idx
    ON graph_preferred_channels (channel_id);

-- Populate graph_preferred_nodes from the graph_nodes rows that already
-- existed before this migration. The inner query ranks every node row within
-- each pub_key group. Announced nodes, identified by a non-empty signature,
-- outrank shell nodes, and higher gossip versions win within the same
-- announced/shell class. The outer INSERT keeps only rn = 1, leaving exactly
-- one preferred node_id per pub_key.
--
-- The conflict clause makes this population step idempotent if the migration
-- is retried after the tables were created and partially populated.
INSERT INTO graph_preferred_nodes (pub_key, node_id)
SELECT sub.pub_key, sub.node_id
FROM (
    SELECT
        n.pub_key,
        n.id AS node_id,
        ROW_NUMBER() OVER (
            PARTITION BY n.pub_key
            ORDER BY
                (COALESCE(length(n.signature), 0) > 0) DESC,
                n.version DESC
        ) AS rn
    FROM graph_nodes n
) sub
WHERE sub.rn = 1
ON CONFLICT (pub_key) DO UPDATE SET node_id = EXCLUDED.node_id
WHERE graph_preferred_nodes.node_id <> EXCLUDED.node_id;

-- Populate graph_preferred_channels from the graph_channels rows that already
-- existed before this migration. The inner query ranks every channel row
-- within each SCID group. A channel version with at least one policy row
-- outranks a bare channel version, and higher gossip versions win within the
-- same policy/bare class. The outer INSERT keeps only rn = 1, leaving exactly
-- one preferred channel_id per SCID.
--
-- The conflict clause makes this population step idempotent if the migration
-- is retried after the tables were created and partially populated.
INSERT INTO graph_preferred_channels (scid, channel_id)
SELECT sub.scid, sub.channel_id
FROM (
    SELECT
        c.scid,
        c.id AS channel_id,
        ROW_NUMBER() OVER (
            PARTITION BY c.scid
            ORDER BY
                EXISTS (
                    SELECT 1 FROM graph_channel_policies p
                    WHERE p.channel_id = c.id
                      AND p.version = c.version
                ) DESC,
                c.version DESC
        ) AS rn
    FROM graph_channels c
) sub
WHERE sub.rn = 1
ON CONFLICT (scid) DO UPDATE SET channel_id = EXCLUDED.channel_id
WHERE graph_preferred_channels.channel_id <> EXCLUDED.channel_id;
