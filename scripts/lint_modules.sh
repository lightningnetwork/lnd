#!/bin/bash

# Accept optional module parameter
TARGET_MODULE=$1

IGNORE="${LINT_IGNORE_MODULES:-tools}"

# If a specific module is provided, lint only that module
if [ -n "$TARGET_MODULE" ]; then
  # Check if the module exists
  if [ ! -f "$TARGET_MODULE/go.mod" ]; then
    echo "Error: Module '$TARGET_MODULE' not found or is not a Go module"
    exit 1
  fi
  SUBMODULES="$TARGET_MODULE"
else
  # Otherwise, lint all submodules
  SUBMODULES=$(find . -mindepth 2 -name "go.mod" | cut -d'/' -f2 | grep -v "$IGNORE")
fi

# Use the same Docker cache mounting strategy as the main Makefile:
# - CI: Use bind mounts to host paths that GitHub Actions caches persist.
# - Local: Use Docker named volumes (much faster on macOS/Windows).
if [ -n "$CI" ]; then
  # CI mode: bind mount to host paths
  DOCKER_CACHE_ARGS="-v ${HOME}/.cache/go-build:/tmp/build/.cache \
    -v ${HOME}/go/pkg/mod:/tmp/build/.modcache \
    -v ${HOME}/.cache/golangci-lint:/root/.cache/golangci-lint"
else
  # Local mode: Docker named volumes for fast macOS/Windows performance
  DOCKER_CACHE_ARGS="-v lnd-go-build-cache:/tmp/build/.cache \
    -v lnd-go-mod-cache:/tmp/build/.modcache \
    -v lnd-go-lint-cache:/root/.cache/golangci-lint"
fi

for submodule in $SUBMODULES
do
  echo "Linting submodule: $submodule"

  # Run linter in submodule directory using Docker
  # Check if .golangci.yml exists in submodule, otherwise use root config
  if [ -f "$submodule/.golangci.yml" ]; then
    docker run --rm \
      $DOCKER_CACHE_ARGS \
      -v $(pwd):/build \
      -w /build/$submodule \
      lnd-tools custom-gcl run -v || exit 1
  else
    docker run --rm \
      $DOCKER_CACHE_ARGS \
      -v $(pwd):/build \
      -w /build/$submodule \
      lnd-tools custom-gcl run -v -c ../.golangci.yml || exit 1
  fi
done
