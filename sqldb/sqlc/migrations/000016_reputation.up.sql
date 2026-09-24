-- reputation_channels holds the persisted local reputation state of each
-- channel, so that a node does not lose the forwarding history its peers have
-- built when it restarts. One row per channel, keyed by short channel id.
--
-- The two decaying averages are stored exactly as they are held in memory: the
-- running value together with the timestamp it was last updated at. Decay is
-- applied lazily on read, so restoring these verbatim means the first read
-- after a restart decays the value over the full downtime.
CREATE TABLE IF NOT EXISTS reputation_channels (
    -- scid is the short channel id of the channel, big endian encoded.
    scid BLOB PRIMARY KEY,

    -- outgoing_reputation is the reputation the channel has accrued as an
    -- outgoing link, in millisatoshis.
    outgoing_reputation BIGINT NOT NULL,

    -- outgoing_reputation_updated_at is the time the outgoing reputation
    -- average was last updated.
    outgoing_reputation_updated_at TIMESTAMP NOT NULL,

    -- incoming_revenue is the revenue the channel has earned as an incoming
    -- link, in millisatoshis, aggregated over several windows.
    incoming_revenue BIGINT NOT NULL,

    -- incoming_revenue_updated_at is the time the incoming revenue average
    -- was last updated.
    incoming_revenue_updated_at TIMESTAMP NOT NULL,

    -- incoming_revenue_started_at is the time the incoming revenue average
    -- started tracking, used for its warm-up factor.
    incoming_revenue_started_at TIMESTAMP NOT NULL
);
