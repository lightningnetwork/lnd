-- name: SetMigration :exec
INSERT INTO
  migration_tracker (version, migration_time) 
VALUES ($1, $2);

-- name: GetMigration :one
SELECT
  migration_time
FROM
  migration_tracker
WHERE
  version = $1;

-- name: GetDatabaseVersion :one
SELECT
  version
FROM
  migration_tracker
ORDER BY
  version DESC
LIMIT 1;
