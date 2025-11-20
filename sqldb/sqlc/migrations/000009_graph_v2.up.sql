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