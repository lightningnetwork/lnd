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

CREATE TABLE IF NOT EXISTS source_nodes (
    node_id BIGINT NOT NULL REFERENCES nodes (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS source_nodes_unique ON source_nodes (
    node_id
);

/* ─────────────────────────────────────────────
   channel data tables
   ─────────────────────────────────────────────
*/

-- channels stores all teh channels that we are aware of in the graph.
CREATE TABLE IF NOT EXISTS channels (
    -- The db ID of the channel.
    id INTEGER PRIMARY KEY,

    -- The protocol version that this node was gossiped on.
    version SMALLINT NOT NULL,

    -- The channel id (short channel id) of the channel.
    scid BLOB NOT NULL,

    -- A reference to a node in the nodes table for the node_1 node in
    -- the channel announcement.
    node_id_1 BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- A reference to a node in the nodes table for the node_2 node in
    -- the channel announcement.
    node_id_2 BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,

    -- The outpoint of the funding transaction. We chose to store this
    -- in a string format for the sake of readability and user queries on
    -- outpoint.
    outpoint TEXT NOT NULL,

    -- The capacity of the channel in millisatoshis. This is nullable since
    -- (for the v1 protocol at least), the capacity is not necessarily known as
    -- it is not gossiped in the channel announcement.
    capacity BIGINT,

    -- bitcoin_key_1 is the public key owned by node_1 that is used to create
    -- a funding transaction. It is nullable since future protocol version may
    -- not make use of this field.
    bitcoin_key_1 BLOB,

    -- bitcoin_key_2 is the public key owned by node_2 that is used to create
    -- the funding transaction. It is nullable since future protocol version may
    -- not make use of this field.
    bitcoin_key_2 BLOB,

    -- node_1_signature is the signature of the serialised channel announcement
    -- using the node_1 public key. It is nullable since future protocol
    -- versions may not make use of this field _and_ because for this node's own
    -- channels, the signature will only be known after the initial record
    -- creation.
    node_1_signature BLOB,

    -- node_2_signature is the signature of the serialised channel announcement
    -- using the node_2 public key. It is nullable since future protocol
    -- versions may not make use of this field _and_ because for this node's own
    -- channels, the signature will only be known after the initial record
    -- creation.
    node_2_signature BLOB,

    -- bitcoin_1_signature is the signature of the serialised channel
    -- announcement using bitcion_key_1. It is nullable since future protocol
    -- versions may not make use of this field _and_ because for this node's
    -- own channels, the signature will only be known after the initial record
    -- creation.
    bitcoin_1_signature BLOB,

    -- bitcoin_2_signature is the signature of the serialised channel
    -- announcement using bitcion_key_2. It is nullable since future protocol
    -- versions may not make use of this field _and_ because for this node's
    -- own channels, the signature will only be known after the initial record
    -- creation.
    bitcoin_2_signature BLOB
);
-- We'll want to lookup all the channels owned by a node, so we create
-- indexes on the node_id_1 and node_id_2 columns.
CREATE INDEX IF NOT EXISTS channels_node_id_1_idx ON channels(node_id_1);
CREATE INDEX IF NOT EXISTS channels_node_id_2_idx ON channels(node_id_2);

-- A channel (identified by a short channel id) can only have one active
-- channel announcement per protocol version. We also order the index by
-- scid in descending order so that we have an idea of the latest channel
-- announcement we know of.
CREATE UNIQUE INDEX IF NOT EXISTS channels_unique ON channels(version, scid DESC);
CREATE INDEX IF NOT EXISTS channels_version_outpoint_idx ON channels(version, outpoint);

-- channel_features contains the feature bits of a channel.
CREATE TABLE IF NOT EXISTS channel_features (
    -- The channel id this feature belongs to.
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,

    -- The feature bit value.
    feature_bit INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS channel_features_unique ON channel_features (
    channel_id, feature_bit
);

-- channel_extra_types stores any extra TLV fields covered by a channels
-- announcement that we do not have an explicit column for in the channels
-- table.
CREATE TABLE IF NOT EXISTS channel_extra_types (
    -- The channel id this TLV field belongs to.
    channel_id BIGINT NOT NULL REFERENCES channels(id) ON DELETE CASCADE,

    -- The Type field.
    type BIGINT NOT NULL,

    -- The value field.
    value BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS channel_extra_types_unique ON channel_extra_types (
    type, channel_id
);
