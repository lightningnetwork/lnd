-- name: UpsertLiquidityInterval :exec
INSERT INTO liquidity_intervals (
    scid, from_node, to_node, lower_ok_msat, upper_fail_msat, estimate_msat,
    confidence_ppm, successes, failures, liquidity_mode, updated_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11
)
ON CONFLICT (scid, from_node, to_node) DO UPDATE SET
    lower_ok_msat = EXCLUDED.lower_ok_msat,
    upper_fail_msat = EXCLUDED.upper_fail_msat,
    estimate_msat = EXCLUDED.estimate_msat,
    confidence_ppm = EXCLUDED.confidence_ppm,
    successes = EXCLUDED.successes,
    failures = EXCLUDED.failures,
    liquidity_mode = EXCLUDED.liquidity_mode,
    updated_at = EXCLUDED.updated_at;

-- name: ListLiquidityIntervals :many
SELECT *
FROM liquidity_intervals
ORDER BY updated_at DESC
LIMIT $1;

-- name: CountLiquidityIntervals :one
SELECT COUNT(*)
FROM liquidity_intervals;

-- name: PruneLiquidityIntervals :exec
DELETE FROM liquidity_intervals
WHERE updated_at < (
    SELECT MIN(updated_at)
    FROM (
        SELECT updated_at
        FROM liquidity_intervals
        ORDER BY updated_at DESC
        LIMIT $1
    ) AS retained
);

-- name: DeleteLiquidityIntervals :exec
DELETE FROM liquidity_intervals;
