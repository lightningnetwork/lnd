#!/bin/bash

set -e

# restore_files is a function to restore original schema files.
restore_files() {
	echo "Restoring SQLite bigint patch..."
	for file in sqldb/sqlc/migrations/*.up.sql.bak; do
		mv "$file" "${file%.bak}"
	done
}


# Set trap to call restore_files on script exit. This makes sure the old files
# are always restored.
trap restore_files EXIT

# Directory of the script file, independent of where it's called from.
DIR="$(cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd)"
# Use the user's cache directories
GOCACHE=`go env GOCACHE`
GOMODCACHE=`go env GOMODCACHE`

# SQLite doesn't support "BIGINT PRIMARY KEY" for auto-incrementing primary
# keys, only "INTEGER PRIMARY KEY". Internally it uses 64-bit integers for
# numbers anyway, independent of the column type. So we can just use
# "INTEGER PRIMARY KEY" and it will work the same under the hood, giving us
# auto incrementing 64-bit integers.
# _BUT_, sqlc will generate Go code with int32 if we use "INTEGER PRIMARY KEY",
# even though we want int64. So before we run sqlc, we need to patch the
# source schema SQL files to use "BIGINT PRIMARY KEY" instead of "INTEGER
# PRIMARY KEY".
echo "Applying SQLite bigint patch..."
for file in sqldb/sqlc/migrations/*.up.sql; do
	echo "Patching $file"
	sed -i.bak -E 's/INTEGER PRIMARY KEY/BIGINT PRIMARY KEY/g' "$file"
done

echo "Generating sql models and queries in go..."

docker run \
  --rm \
  --user "$UID:$(id -g)" \
  -e UID=$UID \
  -v "$DIR/../:/build" \
  -w /build \
  sqlc/sqlc:1.25.0 generate
