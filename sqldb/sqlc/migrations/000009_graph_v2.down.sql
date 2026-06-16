-- Drop v2 block-height indexes.
DROP INDEX IF EXISTS graph_node_block_height_idx;
DROP INDEX IF EXISTS graph_channel_policy_block_height_idx;

-- Restore the original single-column last_update index.
DROP INDEX IF EXISTS graph_node_last_update_idx;
CREATE INDEX IF NOT EXISTS graph_node_last_update_idx ON graph_nodes(last_update);

-- Restore the original single-column channel node-id indexes.
DROP INDEX IF EXISTS graph_channels_node_id_1_idx;
DROP INDEX IF EXISTS graph_channels_node_id_2_idx;
CREATE INDEX IF NOT EXISTS graph_channels_node_id_1_idx ON graph_channels(node_id_1);
CREATE INDEX IF NOT EXISTS graph_channels_node_id_2_idx ON graph_channels(node_id_2);

-- Remove the block_height column from graph_nodes
ALTER TABLE graph_nodes DROP COLUMN block_height;

-- Remove the signature column from graph_channels
ALTER TABLE graph_channels DROP COLUMN signature;

-- Remove the funding_pk_script column from graph_channels
ALTER TABLE graph_channels DROP COLUMN funding_pk_script;

-- Remove the merkle_root_hash column from graph_channels
ALTER TABLE graph_channels DROP COLUMN merkle_root_hash;

-- Remove the block_height column from graph_channel_policies
ALTER TABLE graph_channel_policies DROP COLUMN block_height;

-- Remove the disable_flags column from graph_channel_policies
ALTER TABLE graph_channel_policies DROP COLUMN disable_flags;