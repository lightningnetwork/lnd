#!/bin/bash

SUBMODULES=$(find . -mindepth 2 -name "go.mod" | cut -d'/' -f2)


# Run 'go mod tidy' for root.
go mod tidy

# Run 'go mod tidy' for each module.
for submodule in $SUBMODULES
do
  pushd $submodule

  go mod tidy

  popd
done
