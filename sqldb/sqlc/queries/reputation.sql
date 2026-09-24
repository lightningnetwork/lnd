-- name: UpsertReputationChannel :exec
INSERT INTO reputation_channels (
    scid, outgoing_reputation, outgoing_reputation_updated_at,
    incoming_revenue, incoming_revenue_updated_at, incoming_revenue_started_at
) VALUES (
    $1, $2, $3, $4, $5, $6
)
ON CONFLICT (scid) DO UPDATE SET
    outgoing_reputation = EXCLUDED.outgoing_reputation,
    outgoing_reputation_updated_at = EXCLUDED.outgoing_reputation_updated_at,
    incoming_revenue = EXCLUDED.incoming_revenue,
    incoming_revenue_updated_at = EXCLUDED.incoming_revenue_updated_at,
    incoming_revenue_started_at = EXCLUDED.incoming_revenue_started_at;

-- name: FetchReputationChannels :many
SELECT * FROM reputation_channels
ORDER BY scid;

-- name: DeleteReputationChannel :execrows
DELETE FROM reputation_channels
WHERE scid = $1;
