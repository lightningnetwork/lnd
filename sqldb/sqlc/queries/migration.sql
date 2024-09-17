-- name: UpdateMigration :exec
UPDATE
  migration_tracker
SET
  migration_ts = $2
WHERE
  migration_id = $1;

-- name: GetMigration :one
SELECT
  migration_id,
  migration_ts
FROM
  migration_tracker
WHERE
  migration_id = $1;
