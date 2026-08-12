VERSION_TAG = $(shell date +%Y%m%d)-01
VERSION_CHECK = @$(call print, "Building master with date version tag")

# Create these directories before Docker bind mounts them. Docker creates a
# missing bind-mount source as root, which makes the cache unwritable because
# the release helper deliberately runs as the invoking user.
DOCKER_RELEASE_GOCACHE = $(shell bash -c 'cache="$$($(GOCC) env GOCACHE 2>/dev/null)" || cache=/tmp/go-cache; printf "%s" "$$cache"')
DOCKER_RELEASE_GOMODCACHE = $(shell bash -c 'cache="$$($(GOCC) env GOMODCACHE 2>/dev/null)" || cache=/tmp/go-modcache; printf "%s" "$$cache"')

# A linked worktree has a .git file that points outside the worktree. Mount
# its common Git directory at the same absolute path so tag checks and git
# archive work inside the release helper too.
DOCKER_RELEASE_GIT_COMMON_DIR = $(shell if [ -f .git ]; then git rev-parse --path-format=absolute --git-common-dir; fi)
DOCKER_RELEASE_GIT_MOUNT = $(if $(DOCKER_RELEASE_GIT_COMMON_DIR),-v $(DOCKER_RELEASE_GIT_COMMON_DIR):$(DOCKER_RELEASE_GIT_COMMON_DIR):ro)

define check_docker_release_cache
	@cache="$(1)"; \
	if ! mkdir -p "$$cache"; then \
		echo "error: cannot create Docker release cache: $$cache"; \
		exit 1; \
	fi; \
	cache_ok=1; \
	for shard in $$(printf '%02x\n' $$(seq 0 255)); do \
		shard_dir="$$cache/$$shard"; created=; \
		if [ ! -e "$$shard_dir" ]; then \
			mkdir "$$shard_dir" || { cache_ok=; break; }; created=1; \
		fi; \
		test_dir=$$(mktemp -d "$$shard_dir/.lnd-release-cache.XXXXXX" 2>/dev/null) || { cache_ok=; break; }; \
		rmdir "$$test_dir"; \
		if [ -n "$$created" ] && ! rmdir "$$shard_dir"; then cache_ok=; break; fi; \
	done; \
	if [ -z "$$cache_ok" ]; then \
		echo "error: Docker release cache cannot create directories: $$cache"; \
		echo "hint: remove or chown root-owned files in this cache"; \
		exit 1; \
	fi
endef

DOCKER_RELEASE_HELPER = docker run \
  -it \
  --rm \
  --user $(shell id -u):$(shell id -g) \
  -v $(shell pwd):/tmp/build/lnd \
  $(DOCKER_RELEASE_GIT_MOUNT) \
  -v $(DOCKER_RELEASE_GOCACHE):/tmp/build/.cache \
  -v $(DOCKER_RELEASE_GOMODCACHE):/tmp/build/.modcache \
  -e SKIP_VERSION_CHECK \
  lnd-release-helper

# Please keep this list in sync with .github/workflows/main.yml!
BUILD_SYSTEM = darwin-amd64 \
darwin-arm64 \
freebsd-386 \
freebsd-amd64 \
freebsd-arm \
linux-386 \
linux-amd64 \
linux-armv6 \
linux-armv7 \
linux-arm64 \
netbsd-amd64 \
openbsd-amd64 \
windows-386 \
windows-amd64 \
windows-arm64

RELEASE_TAGS = autopilotrpc signrpc walletrpc chainrpc invoicesrpc watchtowerrpc neutrinorpc monitoring peersrpc kvdb_postgres kvdb_etcd kvdb_sqlite

WASM_RELEASE_TAGS = autopilotrpc signrpc walletrpc chainrpc invoicesrpc watchtowerrpc neutrinorpc monitoring peersrpc

# One can either specify a git tag as the version suffix or one is generated
# from the current date.
ifneq ($(tag),)
VERSION_TAG = $(tag)
VERSION_CHECK = ./scripts/release.sh check-tag "$(VERSION_TAG)"
endif

# By default we will build all systems. But with the 'sys' tag, a specific
# system can be specified. This is useful to release for a subset of
# systems/architectures.
ifneq ($(sys),)
BUILD_SYSTEM = $(sys)
endif

# Use all build tags by default but allow them to be overwritten.
ifneq ($(tags),)
RELEASE_TAGS = $(tags)
endif
