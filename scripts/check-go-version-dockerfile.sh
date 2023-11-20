#!/bin/bash

# Function to check if the Dockerfile contains only the specified Go version.
check_go_version() {
    local dockerfile="$1"
    local required_go_version="$2"

    # Use grep to find lines with 'FROM golang:'
    local go_lines=$(grep -i '^FROM golang:' "$dockerfile")

    # Check if all lines have the required Go version.
    if [ -z "$go_lines" ]; then
        # No Go version found in the file. Skip the check.
        return
    elif echo "$go_lines" | grep -q -v "$required_go_version"; then
        echo "$go_lines"
        echo "Error: $dockerfile does not use Go version $required_go_version exclusively."
        exit 1
    else
        echo "$dockerfile is using Go version $required_go_version."
    fi
}

# Check if the target Go version argument is provided.
if [ $# -eq 0 ]; then
    echo "Usage: $0 <target_go_version>"
    exit 1
fi

target_go_version="$1"

# File paths to be excluded from the check.
exception_list=(
    # Exclude the tools Dockerfile as otherwise the linter may need to be
    # considered every time the Go version is updated.
    "./tools/Dockerfile"
)

# is_exception checks if a file is in the exception list.
is_exception() {
    local file="$1"
    for exception in "${exception_list[@]}"; do
        if [ "$file" == "$exception" ]; then
            return 0
        fi
    done
    return 1
}

# Search for Dockerfiles in the current directory and its subdirectories.
dockerfiles=$(find . -type f -name "*.Dockerfile" -o -name "Dockerfile")

# Check each Dockerfile
for file in $dockerfiles; do
    # Skip the file if it is in the exception list.
    if is_exception "$file"; then
        echo "Skipping $file"
        continue
    fi

    check_go_version "$file" "$target_go_version"
done

echo "All Dockerfiles pass the Go version check for Go version $target_go_version."
