/* ─────────────────────────────────────────────
   node data tables
   ─────────────────────────────────────────────
*/

-- nodes stores all the nodes that we are aware of in the LN graph.
CREATE TABLE IF NOT EXISTS graph_nodes (
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
CREATE UNIQUE INDEX IF NOT EXISTS graph_nodes_unique ON graph_nodes (
    pub_key, version
);
CREATE INDEX IF NOT EXISTS graph_node_last_update_idx ON graph_nodes(last_update);

-- node_extra_types stores any extra TLV fields covered by a node announcement that
-- we do not have an explicit column for in the nodes table.
CREATE TABLE IF NOT EXISTS graph_node_extra_types (
    -- The node id this TLV field belongs to.
    node_id BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

    -- The Type field.
    type BIGINT NOT NULL,

    -- The value field.
    value BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_node_extra_types_unique ON graph_node_extra_types (
    type, node_id
);

-- node_features contains the feature bits of a node.
CREATE TABLE IF NOT EXISTS graph_node_features (
    -- The node id this feature belongs to.
    node_id BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

    -- The feature bit value.
    feature_bit INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_node_features_unique ON graph_node_features (
    node_id, feature_bit
);

-- node_addresses contains the advertised addresses of nodes.
CREATE TABLE IF NOT EXISTS graph_node_addresses (
    -- The node id this feature belongs to.
    node_id BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

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
CREATE UNIQUE INDEX IF NOT EXISTS graph_node_addresses_unique ON graph_node_addresses (
    node_id, type, position
);

CREATE TABLE IF NOT EXISTS graph_source_nodes (
    node_id BIGINT NOT NULL REFERENCES graph_nodes (id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_source_nodes_unique ON graph_source_nodes (
    node_id
);

/* ─────────────────────────────────────────────
   channel data tables
   ─────────────────────────────────────────────
*/

-- channels stores all teh channels that we are aware of in the graph.
CREATE TABLE IF NOT EXISTS graph_channels (
    -- The db ID of the channel.
    id INTEGER PRIMARY KEY,

    -- The protocol version that this node was gossiped on.
    version SMALLINT NOT NULL,

    -- The channel id (short channel id) of the channel.
    scid BLOB NOT NULL,

    -- A reference to a node in the graph_nodes table for the node_1 node in
    -- the channel announcement.
    node_id_1 BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

    -- A reference to a node in the graph_nodes table for the node_2 node in
    -- the channel announcement.
    node_id_2 BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

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
CREATE INDEX IF NOT EXISTS graph_channels_node_id_1_idx ON graph_channels(node_id_1);
CREATE INDEX IF NOT EXISTS graph_channels_node_id_2_idx ON graph_channels(node_id_2);
CREATE INDEX IF NOT EXISTS graph_channels_version_id_idx ON graph_channels(version, id);

-- A channel (identified by a short channel id) can only have one active
-- channel announcement per protocol version. We also order the index by
-- scid in descending order so that we have an idea of the latest channel
-- announcement we know of.
CREATE UNIQUE INDEX IF NOT EXISTS graph_channels_unique ON graph_channels(version, scid DESC);
CREATE INDEX IF NOT EXISTS graph_channels_version_outpoint_idx ON graph_channels(version, outpoint);

-- channel_features contains the feature bits of a channel.
CREATE TABLE IF NOT EXISTS graph_channel_features (
    -- The channel id this feature belongs to.
    channel_id BIGINT NOT NULL REFERENCES graph_channels(id) ON DELETE CASCADE,

    -- The feature bit value.
    feature_bit INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_channel_features_unique ON graph_channel_features (
    channel_id, feature_bit
);

-- channel_extra_types stores any extra TLV fields covered by a channels
-- announcement that we do not have an explicit column for in the channels
-- table.
CREATE TABLE IF NOT EXISTS graph_channel_extra_types (
    -- The channel id this TLV field belongs to.
    channel_id BIGINT NOT NULL REFERENCES graph_channels(id) ON DELETE CASCADE,

    -- The Type field.
    type BIGINT NOT NULL,

    -- The value field.
    value BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_channel_extra_types_unique ON graph_channel_extra_types (
    type, channel_id
);

/* ─────────────────────────────────────────────
   channel policy data tables
   ─────────────────────────────────────────────
*/

CREATE TABLE IF NOT EXISTS graph_channel_policies (
    -- The db ID of the channel policy.
    id INTEGER PRIMARY KEY,

    -- The protocol version that this update was gossiped on.
    version SMALLINT NOT NULL,

    -- The DB ID of the channel that this policy is referencing.
    channel_id BIGINT NOT NULL REFERENCES graph_channels(id) ON DELETE CASCADE,

    -- The DB ID of the node that created the policy update.
    node_id BIGINT NOT NULL REFERENCES graph_nodes(id) ON DELETE CASCADE,

    -- The number of blocks that the node will subtract from the expiry
    -- of an incoming HTLC.
    timelock INTEGER NOT NULL,

    -- The fee rate in parts per million (ppm) that the node will charge
    -- HTLCs for each millionth of a satoshi forwarded.
    fee_ppm BIGINT NOT NULL,

    -- The base fee in millisatoshis that the node will charge for forwarding
    -- any HTLC.
    base_fee_msat BIGINT NOT NULL,

    -- The smallest value HTLC this node will forward.
    min_htlc_msat BIGINT NOT NULL,

    -- The largest value HTLC this node will forward. NOTE: this is nullable
    -- since the field was added later on for the v1 channel update message and
    -- so is not necessarily present in all channel updates.
    max_htlc_msat BIGINT,

    -- The unix timestamp of the last time the policy was updated.
    -- NOTE: this is nullable since in later versions, block-height will likely
    -- be used instead.
    last_update BIGINT,

    -- A boolean indicating that forwards are disabled for this channel.
    -- NOTE: this is nullable since for later protocol versions, this might be
    -- split up into more fine-grained flags.
    disabled bool,

    -- The optional base fee in milli-satoshis that the node will charge
    -- for incoming HTLCs.
    inbound_base_fee_msat BIGINT,

    -- The optional fee rate in parts per million (ppm) that the node will
    -- charge for incoming HTLCs.
    inbound_fee_rate_milli_msat BIGINT,

    -- A bitfield used to provide extra details about the message. This is
    -- nullable since for later protocol versions, this might not be
    -- present. Even though we explicitly store some details from the
    -- message_flags in other ways (for example, max_htlc_msat being not
    -- null means that bit 0 of the message_flags is set), we still store
    -- the full message_flags field so that we can reconstruct the original
    -- announcement if needed, even if the bitfield contains bits that we
    -- don't use or understand.
    message_flags SMALLINT CHECK (message_flags >= 0 AND message_flags <= 255),

    -- A bitfield used to provide extra details about the update. This is
    -- nullable since for later protocol versions, this might not be present.
    -- Even though we explicitly store some details from the channel_flags in
    -- other ways (for example, the disabled field's value is derived directly
    -- from the channel_flags), we still store the full bitfield so that we
    -- can reconstruct the original announcement if needed, even if the
    -- bitfield contains bits that we don't use  or understand.
    channel_flags SMALLINT CHECK (channel_flags >= 0 AND channel_flags <= 255),

    -- The signature of the channel update announcement.
    signature BLOB
);
-- A node can only have a single live policy update for a channel on a
-- given protocol at any given time.
CREATE UNIQUE INDEX IF NOT EXISTS graph_channel_policies_unique ON graph_channel_policies (
    channel_id, node_id, version
);
CREATE INDEX IF NOT EXISTS graph_channel_policy_last_update_idx ON graph_channel_policies(last_update);

-- channel_policy_extra_types stores any extra TLV fields covered by a channel
-- update that we do not have an explicit column for in the channel_policies
-- table.
CREATE TABLE IF NOT EXISTS graph_channel_policy_extra_types (
    -- The channel_policy id this TLV field belongs to.
    channel_policy_id BIGINT NOT NULL REFERENCES graph_channel_policies(id) ON DELETE CASCADE,

    -- The Type field.
    type BIGINT NOT NULL,

    -- The value field.
    value BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_channel_policy_extra_types_unique ON graph_channel_policy_extra_types (
    type, channel_policy_id
);

/* ─────────────────────────────────────────────
   Other graph related tables
   ─────────────────────────────────────────────
*/

CREATE TABLE IF NOT EXISTS graph_zombie_channels (
    -- The channel id (short channel id) of the channel.
    -- NOTE: we don't use a foreign key here to the `channels`
    -- table since we may delete the channel record once it
    -- is marked as a zombie.
    scid BLOB NOT NULL,

    -- The protocol version that this node was gossiped on.
    version SMALLINT NOT NULL,

    -- The public key of the node 1 node of the channel. If
    -- this is not null, it means an update from this node
    -- will be able to resurrect the channel.
    node_key_1 BLOB,

    -- The public key of the node 2 node of the channel. If
    -- this is not null, it means an update from this node
    -- will be able to resurrect the channel.
    node_key_2 BLOB
);
CREATE UNIQUE INDEX IF NOT EXISTS graph_zombie_channels_channel_id_version_idx
    ON graph_zombie_channels(scid, version);

CREATE TABLE IF NOT EXISTS graph_prune_log (
    -- The block height that the prune was performed at.
    -- NOTE: we don't use INTEGER PRIMARY KEY here since that would
    -- get transformed into an auto-incrementing type by our SQL type
    -- replacement logic. We don't want that since this must be the
    -- actual block height and not an auto-incrementing value.
    block_height BIGINT PRIMARY KEY,

    -- The block hash that the prune was performed at.
    block_hash BLOB NOT NULL
);

CREATE TABLE IF NOT EXISTS graph_closed_scids (
    -- The short channel id of the channel.
    scid BLOB PRIMARY KEY
);