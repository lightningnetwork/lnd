#!/bin/bash

set -u

TARGET_MODULE="${1:-}"
MAKE_COMMAND="${LINT_MODULE_MAKE:-make}"
LINT_WORKERS="${LINT_MODULE_WORKERS:-}"

FAILED_MODULES=()
MODULE_COUNT=0

lint_module() {
  local module="$1"
  local lint_config="/build/.golangci.yml"

  if [ -f "$module/.golangci.yml" ]; then
    lint_config="/build/$module/.golangci.yml"
  fi

  local make_args=(
    --no-print-directory
    lint-module-run
    "module=$module"
    "lint_config=$lint_config"
  )

  if [ -n "$LINT_WORKERS" ]; then
    make_args+=("workers=$LINT_WORKERS")
  fi

  MODULE_COUNT=$((MODULE_COUNT + 1))
  echo "Linting submodule: $module"

  if ! "$MAKE_COMMAND" "${make_args[@]}"; then
    FAILED_MODULES+=("$module")
  fi
}

if [ -n "$TARGET_MODULE" ]; then
  # Module paths are relative to the repository root.
  TARGET_MODULE="${TARGET_MODULE#./}"
  TARGET_MODULE="${TARGET_MODULE%/}"

  case "$TARGET_MODULE" in
    ""|/*|..|../*|*/..|*/../*)
      echo "Error: Module must be a repository-relative path"
      exit 1
      ;;
  esac

  if [ ! -f "$TARGET_MODULE/go.mod" ]; then
    echo "Error: Module '$TARGET_MODULE' not found or is not a Go module"
    exit 1
  fi

  lint_module "$TARGET_MODULE"
else
  # Preserve complete module paths so nested modules such as sqldb/v2 are
  # linted independently. The tools modules only contain build dependencies
  # and custom linter implementations, so they are intentionally excluded.
  while IFS= read -r submodule; do
    lint_module "$submodule"
  done < <(
    find . -mindepth 2 -type f -name "go.mod" \
      -not -path "./tools/*" -exec dirname {} \; | \
      sed 's#^\./##' | LC_ALL=C sort -u
  )
fi

if [ "${#FAILED_MODULES[@]}" -ne 0 ]; then
  echo
  echo "Lint failed for ${#FAILED_MODULES[@]} of $MODULE_COUNT submodules:"
  printf '  %s\n' "${FAILED_MODULES[@]}"
  exit 1
fi

echo
echo "Lint passed for all $MODULE_COUNT submodules."
