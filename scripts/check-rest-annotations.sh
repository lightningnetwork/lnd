#!/bin/bash

for proto in $(find . -name "*.proto"); do
  for rpc in $(awk '/    rpc /{print $2}' "$proto"); do
    yaml=${proto%%.proto}.yaml
    if ! grep -q "$rpc" "$yaml"; then
      echo "RPC $rpc not added to $yaml file"
      exit 1
    fi
  done
done
