#!/bin/bash

# DIR is set to the directory of this script.
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# The way we call docker-compose depends on the installation.
if which docker-compose > /dev/null; then
  COMPOSE_CMD="docker-compose"
else
  COMPOSE_CMD="docker compose"
fi

# Common arguments that we want to pass to docker-compose.
# By default, this only includes the main docker-compose file
# and not the override file. Use the `compose_upgrade` method
# to load both docker compose files.
COMPOSE_ARGS="-f $DIR/docker-compose.yaml -p regtest"
COMPOSE="$COMPOSE_CMD $COMPOSE_ARGS"

# compose_upgrade sets COMPOSE_ARGS and COMPOSE such that
# both the main docker-compose file and the override file
# are loaded.
function compose_upgrade() {
  export COMPOSE_ARGS="-p regtest"
  export COMPOSE="$COMPOSE_CMD $COMPOSE_ARGS"
}

# compose_up starts the docker-compose cluster.
function compose_up() {
  echo "🐳 Starting the cluster"
  $COMPOSE up -d --quiet-pull
}

# compose_down tears down the docker-compose cluster
# and removes all volumes and orphans.
function compose_down() {
  echo "🐳 Tearing down the cluster"
  $COMPOSE down --volumes --remove-orphans
}

# compose_stop stops a specific service in the cluster.
function compose_stop() {
  local service="$1"
  echo "🐳 Stopping $service"
  $COMPOSE stop "$service"
}

# compose_start starts a specific service in the cluster.
function compose_start() {
  local service="$1"
  echo "🐳 Starting $service"
  $COMPOSE up -d $service
}

# compose_rebuild forces the rebuild of the image for a
# specific service in the cluster.
function compose_rebuild() {
  local service="$1"
  echo "🐳 Rebuilding $service"
  $COMPOSE build --no-cache $service
}
