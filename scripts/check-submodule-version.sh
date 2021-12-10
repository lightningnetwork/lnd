#!/bin/bash

ROOT_MODULE="github.com/lightningnetwork/lnd"

# The command 'go list -m all' returns all imports in the following format:
#   github.com/lightningnetwork/lnd/cert v1.1.0 => ./cert
# The two cut then first split by spaces and then by slashes to extract the
# submodule names.
SUBMODULES="$(go list -m all | grep $ROOT_MODULE/ | cut -d' ' -f1 | cut -d'/' -f4-)"
BRANCH=$1

for m in $SUBMODULES; do
  has_changes=0
  git diff --stat $BRANCH.. | grep -q " $m/" && has_changes=1
  
  if [[ $has_changes -eq 1 ]]; then
    has_bump=0
    git diff $BRANCH.. -- go.mod | \
      grep -q "^\+[[:space:]]*$ROOT_MODULE/$m " && has_bump=1
    
    if [[ $has_bump -eq 0 ]]; then
      echo "Submodule '$m' has changes but no version bump in go.mod was found"
      echo "If you update code in a submodule, you must bump its version in "
      echo "go.mod to the _next_ version so a tag for that version can be"
      echo "pushed after merging the PR."
      exit 1
    else
      echo "Submodule '$m' has changes but go.mod bumps it to: "
      git diff $BRANCH.. -- go.mod | grep $m
    fi
  else
    echo "Submodule '$m' has no changes, skipping"
  fi
done
