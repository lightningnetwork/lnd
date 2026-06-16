-- The block height timestamp of this node's latest received node announcement.
-- It may be zero if we have not received a node announcement yet.
ALTER TABLE graph_nodes ADD COLUMN block_height BIGINT;

-- The signature of the channel announcement. If this is null, then the channel
-- belongs to the source node and the channel has not been announced yet.
ALTER TABLE graph_channels ADD COLUMN signature BLOB;

-- For v2 channels onwards, we cant necessarily derive the funding pk script
-- from the other fields in the announcement, so we store it here so that
-- we have easy access to it when we want to subscribe to channel closures.
ALTER TABLE graph_channels ADD COLUMN funding_pk_script BLOB;

-- The optional merkel root hash advertised in the V2 channel announcement.
ALTER TABLE graph_channels ADD COLUMN merkle_root_hash BLOB;

-- The block height timestamp of this channel's latest received channel-update
-- message (for v2 channel update messages).
ALTER TABLE graph_channel_policies ADD COLUMN block_height BIGINT;

-- A bitfield describing the disabled flags for a v2 channel update.
ALTER TABLE graph_channel_policies ADD COLUMN disable_flags SMALLINT
    CHECK (disable_flags >= 0 AND disable_flags <= 255);

-- Composite index for v2 node horizon queries. The query filters on
-- (version, block_height) for the range scan and then paginates and orders by
-- (block_height, pub_key). Including pub_key in the index lets the DB cover
-- the ORDER BY without an extra sort and seek directly to the pagination
-- cursor position.
CREATE INDEX IF NOT EXISTS graph_node_block_height_idx
    ON graph_nodes (version, block_height, pub_key);

-- Index for v2 channel policy horizon queries which filter by gossip version
-- and block-height range. The pagination cursor for channel queries uses a
-- CASE expression across two joined policy rows (max of both block_heights),
-- so the index cannot cover the ORDER BY — (version, block_height) is
-- sufficient for the range scan.
CREATE INDEX IF NOT EXISTS graph_channel_policy_block_height_idx
    ON graph_channel_policies (version, block_height);

-- Replace the old single-column last_update index with a composite index
-- that matches the v1 node horizon query shape:
--   WHERE version = 1 AND last_update >= ... ORDER BY last_update, pub_key
DROP INDEX IF EXISTS graph_node_last_update_idx;
CREATE INDEX IF NOT EXISTS graph_node_last_update_idx
    ON graph_nodes(version, last_update, pub_key);

-- Replace the single-column channel node-id indexes with composite indexes
-- that include version. This helps the version-aware public node checks
-- (UNION ALL probes) for both v1 and v2, while still serving node-centric
-- lookups like channel iteration and existence checks.
DROP INDEX IF EXISTS graph_channels_node_id_1_idx;
DROP INDEX IF EXISTS graph_channels_node_id_2_idx;
CREATE INDEX IF NOT EXISTS graph_channels_node_id_1_idx
    ON graph_channels(node_id_1, version);
CREATE INDEX IF NOT EXISTS graph_channels_node_id_2_idx
    ON graph_channels(node_id_2, version);
