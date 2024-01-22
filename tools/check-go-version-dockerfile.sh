#!/bin/bash

# Function to check if the Dockerfile contains only the specified Go version
check_go_version() {
    local dockerfile="$1"
    local required_go_version="$2"

    # Use grep to find lines with 'FROM golang:'
    local go_lines=$(grep -i '^FROM golang:' "$dockerfile")

    # Check if all lines have the required Go version
    if echo "$go_lines" | grep -q -v "$required_go_version"; then
        echo "Error: $dockerfile does not use Go version $required_go_version exclusively."
        exit 1
    else
        echo "$dockerfile is using Go version $required_go_version."
    fi
}

# Check if the target Go version argument is provided
if [ $# -eq 0 ]; then
    echo "Usage: $0 <target_go_version>"
    exit 1
fi

target_go_version="$1"

# Search for Dockerfiles in the current directory and its subdirectories
dockerfiles=$(find . -type f -name "*.Dockerfile" -o -name "Dockerfile")

# Check each Dockerfile
for file in $dockerfiles; do
    check_go_version "$file" "$target_go_version"
done

echo "All Dockerfiles pass the Go version check for Go version $target_go_version."
