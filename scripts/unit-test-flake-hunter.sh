#!/bin/bash

# Check if pkg and case variables are provided.
if [ $# -lt 2 ] || [ $# -gt 3 ]; then
	echo "Usage: $0 <pkg> <case> [timeout]"
	exit 1
fi

pkg=$1
case=$2
timeout=${3:-30s} # Default to 30s if not provided.

counter=0

# Run the command in a loop until it fails.
while output=$(go clean -testcache && make unit-debug log="stdlog trace" pkg=$pkg case=$case timeout=$timeout 2>&1); do
	((counter++))
	echo "Test $case passed, count: $counter"
done

# Only log the output when it fails.
echo "Test $case failed. Output:"
echo "$output"
