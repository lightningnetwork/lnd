#!/bin/bash

IGNORE="tools"
SUBMODULES=$(find . -mindepth 2 -name "go.mod" | cut -d'/' -f2 | grep -v "$IGNORE")

for submodule in $SUBMODULES
do
  pushd $submodule

  echo "Running submodule unit tests in $(pwd)"
  echo "testing $submodule..."
  go test -timeout=5m || exit 1
  
  if [[ "$submodule" == "kvdb" ]]
  then
      echo "testing $submodule with sqlite..."
      go test -tags="kvdb_sqlite" -timeout=5m || exit 1
      
      echo "testing $submodule with postgres..."
      go test -tags="kvdb_postgres" -timeout=5m || exit 1
      
      echo "testing $submodule with etcd..."
      go test -tags="kvdb_etcd" -timeout=5m || exit 1
  fi
  
  popd
done
