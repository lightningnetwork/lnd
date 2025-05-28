-- Drop indexes.
DROP INDEX IF EXISTS nodes_unique;
DROP INDEX IF EXISTS node_extra_types_unique;
DROP INDEX IF EXISTS node_features_unique;
DROP INDEX IF EXISTS node_addresses_unique;
DROP INDEX IF EXISTS source_nodes_unique;

-- Drop tables in order of reverse dependencies.
DROP TABLE IF EXISTS source_nodes;
DROP TABLE IF EXISTS node_addresses;
DROP TABLE IF EXISTS node_features;
DROP TABLE IF EXISTS node_extra_types;
DROP TABLE IF EXISTS nodes;