VERSION_TAG = $(shell date +%Y%m%d)-01
VERSION_CHECK = @$(call print, "Building master with date version tag")

DOCKER_RELEASE_HELPER = docker run \
  -it \
  --rm \
  --user $(shell id -u):$(shell id -g) \
  -v $(shell pwd):/tmp/build/lnd \
  -v $(shell bash -c "$(GOCC) env GOCACHE || (mkdir -p /tmp/go-cache; echo /tmp/go-cache)"):/tmp/build/.cache \
  -v $(shell bash -c "$(GOCC) env GOMODCACHE || (mkdir -p /tmp/go-modcache; echo /tmp/go-modcache)"):/tmp/build/.modcache \
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
windows-arm

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
