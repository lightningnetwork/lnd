---
name: bump-golang-version
description: Updates Go version across lnd codebase. Use when bumping Go toolchain version or minimum Go version. Handles Dockerfiles, GitHub workflows, go.mod files, and documentation.
allowed-tools: Read, Edit, Grep, Glob, Bash
---

# Bump Golang Version

This skill handles updating Go versions across the lnd codebase. There are two types of version updates that typically happen together.

## Overview

The lnd project uses two different Go versions:
1. **Toolchain Version** (e.g., 1.25.5) - Used for builds, CI/CD, and Docker images
2. **Minimum Go Version** (e.g., 1.24.11) - Minimum required version in go.mod files

**Important**: LND supports the last two major Go versions. The minimum go.mod version is typically one major version behind the toolchain version (e.g., toolchain 1.25.x with minimum 1.24.x).

## Instructions

### Step 1: Determine Version Type

Ask the user which version(s) they want to update:
- Toolchain version only (build/CI)
- Minimum go.mod version only
- Both (most common)

Get the new version number(s) from the user.

### Step 2: Update Toolchain Version (if requested)

Update the **toolchain version** in these files (use Grep to find current version first):

#### Core Build Files
1. **Makefile**: Update `GO_VERSION = X.XX.X`
2. **.github/workflows/main.yml**: Update `GO_VERSION: X.XX.X` in the env section
3. **.github/workflows/release.yaml**: Update `GO_VERSION: X.XX.X`
4. **.golangci.yml**: Update version reference

#### Docker Files (Format: `FROM golang:X.XX.X-alpine`)
5. **Dockerfile**
6. **dev.Dockerfile**
7. **docker/btcd/Dockerfile**
8. **lnrpc/Dockerfile**
9. **make/builder.Dockerfile**
10. **tools/Dockerfile**

#### Scripts
11. **lnrpc/gen_protos_docker.sh**: Update golang image version

**Note**: .github/actions/setup-go/action.yml contains version references in comments for the cache key examples, update those too.

### Step 3: Update Minimum Go Version (if requested)

Update the **minimum go version** in go.mod files:

#### Go Module Files (Format: `go X.XX.XX`)
1. **go.mod** (main)
2. **healthcheck/go.mod**
3. **kvdb/go.mod**
4. **sqldb/go.mod**
5. **tlv/go.mod**
6. **tools/go.mod**
7. **tools/linters/go.mod**
8. **tor/go.mod**

**Important**: The minimum version is typically one major version behind the toolchain to support the last two major Go versions (e.g., toolchain 1.25.5, minimum 1.24.11).

### Step 4: Update Documentation

Update **docs/INSTALL.md**:
- Update minimum version text (e.g., "minimum version of `1.24.11`")
- Update all download URLs for different architectures:
  - linux-amd64
  - linux-armv6l
  - (check file for complete list)
- **CRITICAL**: Update SHA256 checksums for each download URL
  - You MUST fetch the correct checksums from https://go.dev/dl/
  - DO NOT skip this step or leave incorrect checksums

### Step 5: Verify Changes

1. Search for any remaining references to old versions:
   ```bash
   git grep -n "OLD_VERSION"
   ```

2. Run the linter to check for issues:
   ```bash
   make lint
   ```

3. Verify that files match the pattern from previous commits

### Step 6: Create Commits

Create two separate commits following the project's convention:

**Commit 1: Toolchain Version Update**
```
build: update CI+release version to Go X.XX.X

This updates the toolchain which we require to build the LND
executable.
```

**Commit 2: Minimum Go Version Update**
```
mod: update the minimum go version to X.XX.XX

We update the minimum required go version to build the LND
executable to the latest minor release of go X.XX.XX.
```

## Important Notes

- **LND supports the last two major Go versions** - minimum version is one major version behind toolchain
- All comments in the codebase mention "If you change this please also update GO_VERSION in Makefile (then run `make lint` to see where else it needs to be updated as well)"
- Always verify you've found all references by searching the codebase
- docs/INSTALL.md requires SHA256 checksums - these MUST be fetched from go.dev/dl/

## Example Workflow

```
User: "Bump Go toolchain to 1.25.6 and minimum to 1.24.12"
Assistant:
1. Greps for current version in Makefile
2. Updates all 12 toolchain files to 1.25.6
3. Updates all 9 go.mod files to 1.24.12
4. Updates docs/INSTALL.md with new checksums
5. Verifies with git grep
6. Runs make lint
7. Creates two commits with proper messages
```
