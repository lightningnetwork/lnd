-- Drop indexes.
DROP INDEX IF EXISTS graph_nodes_unique;
DROP INDEX IF EXISTS graph_node_extra_types_unique;
DROP INDEX IF EXISTS graph_node_features_unique;
DROP INDEX IF EXISTS graph_node_addresses_unique;
DROP INDEX IF EXISTS graph_node_last_update_idx;
DROP INDEX IF EXISTS graph_source_nodes_unique;
DROP INDEX IF EXISTS graph_channels_node_id_1_idx;
DROP INDEX IF EXISTS graph_channels_node_id_2_idx;
DROP INDEX IF EXISTS graph_channels_unique;
DROP INDEX IF EXISTS graph_channels_version_outpoint_idx;
DROP INDEX IF EXISTS graph_channels_version_id_idx;
DROP INDEX IF EXISTS graph_channel_features_unique;
DROP INDEX IF EXISTS graph_channel_extra_types_unique;
DROP INDEX IF EXISTS graph_channel_policies_unique;
DROP INDEX IF EXISTS graph_channel_policy_extra_types_unique;
DROP INDEX IF EXISTS graph_channel_policy_last_update_idx;
DROP INDEX IF EXISTS graph_zombie_channels_channel_id_version_idx;

-- Drop tables in order of reverse dependencies.
DROP TABLE IF EXISTS graph_channel_policy_extra_types;
DROP TABLE IF EXISTS graph_channel_policies;
DROP TABLE IF EXISTS graph_channel_features;
DROP TABLE IF EXISTS graph_channel_extra_types;
DROP TABLE IF EXISTS graph_channels;
DROP TABLE IF EXISTS graph_source_nodes;
DROP TABLE IF EXISTS graph_node_addresses;
DROP TABLE IF EXISTS graph_node_features;
DROP TABLE IF EXISTS graph_node_extra_types;
DROP TABLE IF EXISTS graph_nodes;
DROP TABLE IF EXISTS graph_channel_policy_extra_types;
DROP TABLE IF EXISTS graph_zombie_channels;
DROP TABLE IF EXISTS graph_prune_log;
DROP TABLE IF EXISTS graph_closed_scids;

