#!/bin/bash

# Function to check if the YAML file contains the specified Go version after
# field 'go:'.
check_go_version_yaml() {
    local yamlfile="$1"
    local required_go_version="$2"

    # Use grep to find lines with 'go:'. The grep exist status is ignored.
    local go_lines=$(grep -i '^\s*go:\s*"[0-9]\+\.[0-9]\+\(\.[0-9]\+\)\?"' "$yamlfile" || true)

    # Check if any lines specify the Go version.
    if [ -n "$go_lines" ]; then
        # Extract the Go version from the file's lines. Example matching strings:
        # go: "1.21.0"
        local extracted_go_version=$(echo "$go_lines" | sed -n 's/.*go: "\([^"]*\)".*/\1/p')

        # Check if the extracted Go version matches the required version.
        if [ "$extracted_go_version" != "$required_go_version" ]; then
            echo "Error finding pattern 'go:': $yamlfile specifies Go version '$extracted_go_version', but required version is '$required_go_version'."
            exit 1
        else
            echo "$yamlfile specifies Go version $required_go_version."
        fi
    fi
}

# Function to check if the YAML file contains the specified Go version after
# environment variable 'GO_VERSION:'.
check_go_version_env_variable() {
    local yamlfile="$1"
    local required_go_version="$2"

    # Use grep to find lines with 'GO_VERSION:'. The grep exist status is
    # ignored.
    local go_lines=$(grep -i 'GO_VERSION:' "$yamlfile" || true)

    # Check if any lines specify the Go version.
    if [ -n "$go_lines" ]; then
        # Extract the Go version from the file's lines. Example matching strings:
        # GO_VERSION: "1.21.0"
        # GO_VERSION: '1.21.0'
        # GO_VERSION: 1.21.0
        # GO_VERSION:1.21.0
        #   GO_VERSION:1.21.0
        local extracted_go_version=$(echo "$go_lines" | sed -n 's/.*GO_VERSION[: ]*["'\'']*\([0-9.]*\).*/\1/p')

        # Check if the extracted Go version matches the required version.
        if [ "$extracted_go_version" != "$required_go_version" ]; then
            echo "Error finding pattern 'GO_VERSION:': $yamlfile specifies Go version '$extracted_go_version', but required version is '$required_go_version'."
            exit 1
        else
            echo "$yamlfile specifies Go version $required_go_version."
        fi
    fi
}

# Check if the target Go version argument is provided.
if [ $# -eq 0 ]; then
    echo "Usage: $0 <target_go_version>"
    exit 1
fi

target_go_version="$1"

# Search for YAML files in the current directory and its subdirectories.
yaml_files=$(find . -type f \( -name "*.yaml" -o -name "*.yml" \))

# Check each YAML file.
for file in $yaml_files; do
    check_go_version_yaml "$file" "$target_go_version"
    check_go_version_env_variable "$file" "$target_go_version"
done

echo "All YAML files pass the Go version check for Go version $target_go_version."
