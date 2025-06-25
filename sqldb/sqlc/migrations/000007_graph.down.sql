-- Drop indexes.
DROP INDEX IF EXISTS nodes_unique;
DROP INDEX IF EXISTS node_extra_types_unique;
DROP INDEX IF EXISTS node_features_unique;
DROP INDEX IF EXISTS node_addresses_unique;
DROP INDEX IF EXISTS node_last_update_idx;
DROP INDEX IF EXISTS source_nodes_unique;
DROP INDEX IF EXISTS channels_node_id_1_idx;
DROP INDEX IF EXISTS channels_node_id_2_idx;
DROP INDEX IF EXISTS channels_unique;
DROP INDEX IF EXISTS channels_version_outpoint_idx;
DROP INDEX IF EXISTS channel_features_unique;
DROP INDEX IF EXISTS channel_extra_types_unique;
DROP INDEX IF EXISTS channel_policies_unique;
DROP INDEX IF EXISTS channel_policy_extra_types_unique;
DROP INDEX IF EXISTS channel_policy_last_update_idx;

-- Drop tables in order of reverse dependencies.
DROP TABLE IF EXISTS channel_policy_extra_types;
DROP TABLE IF EXISTS channel_policies;
DROP TABLE IF EXISTS channel_features;
DROP TABLE IF EXISTS channel_extra_types;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS source_nodes;
DROP TABLE IF EXISTS node_addresses;
DROP TABLE IF EXISTS node_features;
DROP TABLE IF EXISTS node_extra_types;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS channel_policy_extra_types;
DROP TABLE IF EXISTS zombie_channels;
DROP TABLE IF EXISTS prune_log;
DROP TABLE IF EXISTS closed_scids;
