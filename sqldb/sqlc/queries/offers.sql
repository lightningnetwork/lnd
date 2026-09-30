-- name: InsertOffer :one
INSERT INTO offers (
    hash, encoded, is_disabled, created_at
) VALUES (
    $1, $2, $3, $4
) RETURNING id;

-- name: GetOfferByHash :one
SELECT *
FROM offers
WHERE hash = $1;
