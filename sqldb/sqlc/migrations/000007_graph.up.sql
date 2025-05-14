/* ─────────────────────────────────────────────
   node data tables
   ─────────────────────────────────────────────
*/

-- nodes stores all the nodes that we are aware of in the LN graph.
CREATE TABLE IF NOT EXISTS nodes (
    -- The db ID of the node. This will only be used DB level
    -- relations.
    id INTEGER PRIMARY KEY,

    -- The protocol version that this node was gossiped on.
    version SMALLINT NOT NULL,

    -- The public key (serialised compressed) of the node.
    pub_key BLOB NOT NULL,

    -- The alias of the node.
    alias TEXT,

    -- The unix timestamp of the last time the node was updated.
    last_update BIGINT,

    -- The color of the node.
    color VARCHAR,

    -- The signature of the node announcement. If this is null, then
    -- the node announcement has not been received yet and this record
    -- is a shell node. This can be the case if we receive a channel
    -- announcement for a channel that is connected to a node that we
    -- have not yet received a node announcement for.
    signature BLOB
);

-- A node (identified by a public key) can only have one active node
-- announcement per protocol.
CREATE UNIQUE INDEX IF NOT EXISTS nodes_unique ON nodes (
    pub_key, version
);

-- node_extra_types stores any extra TLV fields covered by a node announcement that
-- we do not have an explicit column for in the nodes table.
CREATE TABLE IF NOT EXISTS node_extra_types (
    -- The node id this TLV field belongs to.
    node_id BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- The Type field.
    type BIGINT NOT NULL,

    -- The value field.
    value BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS node_extra_types_unique ON node_extra_types (
    type, node_id
);

-- node_features contains the feature bits of a node.
CREATE TABLE IF NOT EXISTS node_features (
    -- The node id this feature belongs to.
    node_id BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- The feature bit value.
    feature_bit INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS node_features_unique ON node_features (
    node_id, feature_bit
);

-- node_addresses contains the advertised addresses of nodes.
CREATE TABLE IF NOT EXISTS node_addresses (
    -- The node id this feature belongs to.
    node_id BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- An enum that represents the type of address. This will
    -- dictate how the address column should be parsed.
    type SMALLINT NOT NULL,

    -- position is position of this address in the list of addresses
    -- under the given type as it appeared in the node announcement.
    -- We need to store this so that when we reconstruct the node
    -- announcement, we preserve the original order of the addresses
    -- so that the signature of the announcement remains valid.
    position INTEGER NOT NULL,

    -- The advertised address of the node.
    address TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS node_addresses_unique ON node_addresses (
    node_id, type, position
);