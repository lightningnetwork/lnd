PKG := github.com/lightningnetwork/lnd
MOBILE_PKG := $(PKG)/mobile
TOOLS_DIR := tools
TOOLS_MOD := $(TOOLS_DIR)/go.mod

GOCC ?= go
PREFIX ?= /usr/local

GOTOOL := GOWORK=off $(GOCC) tool -modfile=$(TOOLS_MOD)


BTCD_PKG := github.com/btcsuite/btcd
GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports
GOLINT_PKG := github.com/golangci/golangci-lint/v2/cmd/golangci-lint

GO_BIN := ${GOPATH}/bin
BTCD_BIN := $(GO_BIN)/btcd
GOMOBILE_BIN := $(GO_BIN)/gomobile

MOBILE_BUILD_DIR :=${GOPATH}/src/$(MOBILE_PKG)/build
IOS_BUILD_DIR := $(MOBILE_BUILD_DIR)/ios
IOS_BUILD := $(IOS_BUILD_DIR)/Lndmobile.xcframework
ANDROID_BUILD_DIR := $(MOBILE_BUILD_DIR)/android
ANDROID_BUILD := $(ANDROID_BUILD_DIR)/Lndmobile.aar

COMMIT := $(shell git describe --tags --dirty)

# Determine the minor version of the active Go installation.
ACTIVE_GO_VERSION := $(shell $(GOCC) version | sed -nre 's/^[^0-9]*(([0-9]+\.)*[0-9]+).*/\1/p')
ACTIVE_GO_VERSION_MINOR := $(shell echo $(ACTIVE_GO_VERSION) | cut -d. -f2)

# GO_VERSION is the Go version used for the release build, docker files, and
# GitHub Actions. This is the reference version for the project. All other Go
# versions are checked against this version.
GO_VERSION = 1.25.3

GOBUILD := $(GOCC) build -v
GOINSTALL := $(GOCC) install -v
GOTEST := $(GOCC) test

GOFILES_NOVENDOR = $(shell find . -type f -name '*.go' -not -path "./vendor/*" -not -name "*pb.go" -not -name "*pb.gw.go" -not -name "*.pb.json.go")

RM := rm -f
CP := cp
MAKE := make
XARGS := xargs -L 1

include make/testing_flags.mk
include make/release_flags.mk
include make/fuzz_flags.mk

DEV_TAGS := $(if ${tags},$(DEV_TAGS) ${tags},$(DEV_TAGS))

# We only return the part inside the double quote here to avoid escape issues
# when calling the external release script. The second parameter can be used to
# add additional ldflags if needed (currently only used for the release).
make_ldflags = $(1) -X $(PKG)/build.Commit=$(COMMIT)

DEV_GCFLAGS := -gcflags "all=-N -l"
DEV_LDFLAGS := -ldflags "$(call make_ldflags)"
# For the release, we want to remove the symbol table and debug information (-s)
# and omit the DWARF symbol table (-w). Also we clear the build ID.
RELEASE_LDFLAGS := $(call make_ldflags, -s -w -buildid=)

# Linting uses a lot of memory, so keep it under control by limiting the number
# of workers if requested.
ifneq ($(workers),)
LINT_WORKERS = --concurrency=$(workers)
endif

# Docker cache mounting strategy:
# - CI (GitHub Actions): Use bind mounts to host paths that GA caches persist.
# - Local: Use Docker named volumes (much faster on macOS/Windows due to
#   avoiding slow host-syncing overhead).
# Paths inside container must match GOCACHE/GOMODCACHE in tools/Dockerfile.
ifdef CI
# CI mode: bind mount to host paths that GitHub Actions caches.
DOCKER_TOOLS = docker run \
  --rm \
  -v $${HOME}/.cache/go-build:/tmp/build/.cache \
  -v $${HOME}/go/pkg/mod:/tmp/build/.modcache \
  -v $${HOME}/.cache/golangci-lint:/root/.cache/golangci-lint \
  -v $$(pwd):/build lnd-tools
else
# Local mode: Docker named volumes for fast macOS/Windows performance.
DOCKER_TOOLS = docker run \
  --rm \
  -v lnd-go-build-cache:/tmp/build/.cache \
  -v lnd-go-mod-cache:/tmp/build/.modcache \
  -v lnd-go-lint-cache:/root/.cache/golangci-lint \
  -v $$(pwd):/build lnd-tools
endif

GREEN := "\\033[0;32m"
NC := "\\033[0m"
define print
	echo $(GREEN)$1$(NC)
endef

default: scratch

all: scratch check install

# ============
# DEPENDENCIES
# ============
$(BTCD_BIN):
	@$(call print, "Installing btcd.")
	cd $(TOOLS_DIR); $(GOCC) install -trimpath $(BTCD_PKG)

# ============
# INSTALLATION
# ============

#? build: Build lnd and lncli binaries, place them in project directory
build:
	@$(call print, "Building debug lnd and lncli.")
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lnd-debug $(DEV_GCFLAGS) $(DEV_LDFLAGS) $(PKG)/cmd/lnd
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lncli-debug $(DEV_GCFLAGS) $(DEV_LDFLAGS) $(PKG)/cmd/lncli

#? build-itest: Build integration test binaries, place them in itest directory
build-itest:
	@$(call print, "Building itest btcd and lnd.")
	CGO_ENABLED=0 $(GOBUILD) -tags="integration" -o itest/btcd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(BTCD_PKG)
	CGO_ENABLED=0 $(GOBUILD) -tags="$(ITEST_TAGS)" $(ITEST_COVERAGE) -o itest/lnd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(PKG)/cmd/lnd

	@$(call print, "Building itest binary for ${backend} backend.")
	CGO_ENABLED=0 $(GOTEST) -v ./itest -tags="$(DEV_TAGS) $(RPC_TAGS) integration $(backend)" -c -o itest/itest.test$(EXEC_SUFFIX)

#? build-itest-race: Build integration test binaries in race detector mode, place them in itest directory
build-itest-race:
	@$(call print, "Building itest btcd and lnd with race detector.")
	CGO_ENABLED=0 $(GOBUILD) -tags="integration" -o itest/btcd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(BTCD_PKG)
	CGO_ENABLED=1 $(GOBUILD) -race -tags="$(ITEST_TAGS)" -o itest/lnd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(PKG)/cmd/lnd

	@$(call print, "Building itest binary for ${backend} backend.")
	CGO_ENABLED=0 $(GOTEST) -v ./itest -tags="$(DEV_TAGS) $(RPC_TAGS) integration $(backend)" -c -o itest/itest.test$(EXEC_SUFFIX)

#? install-binaries: Build and install lnd and lncli binaries, place them in $GOPATH/bin
install-binaries:
	@$(call print, "Installing lnd and lncli.")
	$(GOINSTALL) -tags="${tags}" -ldflags="$(RELEASE_LDFLAGS)" $(PKG)/cmd/lnd
	$(GOINSTALL) -tags="${tags}" -ldflags="$(RELEASE_LDFLAGS)" $(PKG)/cmd/lncli

#? manpages: generate and install man pages
manpages:
	@$(call print, "Generating man pages lncli.1 and lnd.1.")
	./scripts/gen_man_pages.sh $(DESTDIR) $(PREFIX)

#? install: Build and install lnd and lncli binaries and place them in $GOPATH/bin.
install: install-binaries

#? install-all: Performs all the same tasks as the install command along with generating and installing the man pages for the lnd and lncli binaries. This command is useful in an environment where a user has root access and so has write access to the man page directory.
install-all: install manpages

#? release-install: Build and install lnd and lncli release binaries, place them in $GOPATH/bin
release-install:
	@$(call print, "Installing release lnd and lncli.")
	env CGO_ENABLED=0 $(GOINSTALL) -v -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" $(PKG)/cmd/lnd
	env CGO_ENABLED=0 $(GOINSTALL) -v -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" $(PKG)/cmd/lncli

#? cross-release-install: Build lnd and lncli release binaries for single/all supported platforms to /tmp (useful for checking cross compilation or priming release build cache).
cross-release-install:
	@$(call print, "Cross compiling release lnd and lncli.")
	for sys in $(BUILD_SYSTEM); do \
		echo "Building lnd and lncli for $$sys"; \
		export CGO_ENABLED=0 GOOS=$$(echo $$sys | cut -d- -f1) GOARCH=$$(echo $$sys | cut -d- -f2); \
		if [ "$$GOARCH" = "armv6" ]; then \
			export GOARCH=arm; GOARM=6; \
		elif [ "$$GOARCH" = "armv7" ]; then \
			export GOARCH=arm; GOARM=7; \
		fi; \
		$(GOBUILD) -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" -o /tmp/lnd-$$sys $(PKG)/cmd/lnd; \
		$(GOBUILD) -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" -o /tmp/lncli-$$sys $(PKG)/cmd/lncli; \
		echo; \
	done

#? release: Build the full set of reproducible release binaries for all supported platforms. Make sure the generated mobile RPC stubs don't influence our vendor package by removing them first in the clean-mobile target.
release: clean-mobile
	@$(call print, "Releasing lnd and lncli binaries.")
	$(VERSION_CHECK)
	./scripts/release.sh build-release "$(VERSION_TAG)" "$(BUILD_SYSTEM)" "$(RELEASE_TAGS)" "$(RELEASE_LDFLAGS)" "$(GO_VERSION)"

#? docker-release: Same as release but within a docker container to support reproducible builds on BSD/MacOS platforms
docker-release:
	@$(call print, "Building release helper docker image.")
	if [ "$(tag)" = "" ]; then echo "Must specify tag=<commit_or_tag>!"; exit 1; fi

	docker build -t lnd-release-helper -f make/builder.Dockerfile make/

	# Run the actual compilation inside the docker image. We pass in all flags
	# that we might want to overwrite in manual tests.
	$(DOCKER_RELEASE_HELPER) make release tag="$(tag)" sys="$(sys)" COMMIT="$(COMMIT)" 

docker-tools:
	@$(call print, "Building tools docker image.")
	docker build -q -t lnd-tools $(TOOLS_DIR)

scratch: build


# =======
# TESTING
# =======

#? check: Run unit and integration tests
check: unit itest

db-instance:
ifeq ($(dbbackend),postgres)
	# Remove a previous postgres instance if it exists.
	docker rm lnd-postgres --force || echo "Starting new postgres container"

	# Start a fresh postgres instance. Allow a maximum of 500 connections so
	# that multiple lnd instances with a maximum number of connections of 20
	# each can run concurrently. Note that many of the settings here are
	# specifically for integration testing and are not fit for running
	# production nodes. The increase in max connections ensures that there
	# are enough entries allocated for the RWConflictPool to allow multiple
	# conflicting transactions to track serialization conflicts. The
	# increase in predicate locks and locks per transaction is to allow the
	# queries to lock individual rows instead of entire tables, helping
	# reduce serialization conflicts. Disabling sequential scan for small
	# tables also helps prevent serialization conflicts by ensuring lookups
	# lock only relevant rows in the index rather than the entire table.
	docker run --name lnd-postgres -e POSTGRES_PASSWORD=postgres -p 6432:5432 -d postgres:13-alpine -N 1500 -c max_pred_locks_per_transaction=1024 -c max_locks_per_transaction=128 -c enable_seqscan=off
	docker logs -f lnd-postgres >itest/postgres.log 2>&1 &

	# Wait for the instance to be started.
	sleep $(POSTGRES_START_DELAY)
endif

clean-itest-logs:
	rm -rf itest/*.log itest/.logs-*

#? itest-only: Only run integration tests without re-building binaries
itest-only: clean-itest-logs db-instance
	@$(call print, "Running integration tests with ${backend} backend.")
	date
	EXEC_SUFFIX=$(EXEC_SUFFIX) scripts/itest_part.sh 0 1 $(SHUFFLE_SEED) $(TEST_FLAGS) $(ITEST_FLAGS) -test.v
	$(COLLECT_ITEST_COVERAGE)

#? itest: Build and run integration tests
itest: build-itest itest-only

#? itest-race: Build and run integration tests in race detector mode
itest-race: build-itest-race itest-only

#? itest-parallel: Build and run integration tests in parallel mode, running up to ITEST_PARALLELISM test tranches in parallel (default 4)
itest-parallel: clean-itest-logs build-itest db-instance
	@$(call print, "Running tests")
	date
	EXEC_SUFFIX=$(EXEC_SUFFIX) scripts/itest_parallel.sh $(ITEST_PARALLELISM) $(NUM_ITEST_TRANCHES) $(SHUFFLE_SEED) $(TEST_FLAGS) $(ITEST_FLAGS)
	$(COLLECT_ITEST_COVERAGE)

#? itest-clean: Kill all running itest processes
itest-clean:
	@$(call print, "Cleaning old itest processes")
	killall lnd-itest || echo "no running lnd-itest process found";

#? unit: Run unit tests
unit: $(BTCD_BIN)
	@$(call print, "Running unit tests.")
	$(UNIT)

#? unit-module: Run unit tests of all submodules
unit-module:
	@$(call print, "Running submodule unit tests.")
	scripts/unit_test_modules.sh

#? unit-debug: Run unit tests with debug log output enabled
unit-debug: $(BTCD_BIN)
	@$(call print, "Running debug unit tests.")
	$(UNIT_DEBUG)

#? unit-cover: Run unit tests in coverage mode
unit-cover: $(BTCD_BIN)
	@$(call print, "Running unit coverage tests.")
	$(UNIT_COVER)

#? unit-race: Run unit tests in race detector mode
unit-race: $(BTCD_BIN)
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

#? unit-bench: Run benchmark tests
unit-bench: $(BTCD_BIN)
	@$(call print, "Running benchmark tests.")
	$(UNIT_BENCH)

# =============
# FLAKE HUNTING
# =============

#? flakehunter-itest: Run the integration tests continuously until one fails
flakehunter-itest: build-itest
	@$(call print, "Flake hunting ${backend} integration tests.")
	while [ $$? -eq 0 ]; do make itest-only icase='${icase}' backend='${backend}'; done

#? flakehunter-unit: Run the unit tests continuously until one fails
flakehunter-unit:
	@$(call print, "Flake hunting unit test.")
	scripts/unit-test-flake-hunter.sh ${pkg} ${case}

#? flakehunter-unit-all: Run all unit tests continuously until one fails
flakehunter-unit-all: $(BTCD_BIN)
	@$(call print, "Flake hunting unit tests.")
	while [ $$? -eq 0 ]; do make unit; done

#? flakehunter-unit-race: Run all unit tests in race detector mode continuously until one fails
flakehunter-unit-race: $(BTCD_BIN)
	@$(call print, "Flake hunting unit tests in race detector mode.")
	while [ $$? -eq 0 ]; do make unit-race; done

#? flakehunter-itest-parallel: Run the integration tests continuously until one fails, running up to ITEST_PARALLELISM test tranches in parallel (default 4)
flakehunter-itest-parallel:
	@$(call print, "Flake hunting ${backend} integration tests in parallel.")
	while [ $$? -eq 0 ]; do make itest-parallel tranches=1 parallel=${ITEST_PARALLELISM} icase='${icase}' backend='${backend}'; done

# =============
# FUZZING
# =============

#? fuzz: Run the fuzzing tests
fuzz:
	@$(call print, "Fuzzing packages '$(FUZZPKG)'.")
	scripts/fuzz.sh run "$(FUZZPKG)" "$(FUZZ_TEST_RUN_TIME)" "$(FUZZ_NUM_PROCESSES)"

# =========
# UTILITIES
# =========

#? fmt: Format source code and fix imports
fmt:
	@$(call print, "Fixing imports.")
	$(GOTOOL) $(GOIMPORTS_PKG) -w $(GOFILES_NOVENDOR) 
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

#? fmt-check: Make sure source code is formatted and imports are correct
fmt-check: fmt
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

#? check-go-version-yaml: Verify that the Go version is correct in all YAML files
check-go-version-yaml:
	@$(call print, "Checking for target Go version (v$(GO_VERSION)) in YAML files (*.yaml, *.yml)")
	./scripts/check-go-version-yaml.sh $(GO_VERSION)

#? check-go-version-dockerfile: Verify that the Go version is correct in all Dockerfile files
check-go-version-dockerfile:
	@$(call print, "Checking for target Go version (v$(GO_VERSION)) in Dockerfile files (*Dockerfile)")
	./scripts/check-go-version-dockerfile.sh $(GO_VERSION)

#? check-go-version: Verify that the Go version is correct in all project files
check-go-version: check-go-version-dockerfile check-go-version-yaml

#? lint-source: Run static code analysis
lint-source: docker-tools	
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) custom-gcl run -v $(LINT_WORKERS)

#? lint-config-check: Verify that the lint config is up to date
#  We use the official linter here not our custom one because for checking the
#  config file it does not matter.
lint-config-check:
	@$(call print, "Checking lint config is up to date.")
	$(GOTOOL) $(GOLINT_PKG) config verify -v

#? lint: Run static code analysis
lint: check-go-version lint-config-check lint-source 

#? protolint: Lint proto files using protolint
protolint:
	@$(call print, "Linting proto files.")
	docker run --rm --volume "$$(pwd):/workspace" --workdir /workspace yoheimuta/protolint lint lnrpc/

#? tidy-module: Run `go mod` tidy for all modules
tidy-module:
	echo "Running 'go mod tidy' for all modules"
	scripts/tidy_modules.sh

#? tidy-module-check: Make sure all modules are up to date
tidy-module-check: tidy-module
	if test -n "$$(git status --porcelain)"; then echo "modules not updated, please run `make tidy-module` again!"; git status; exit 1; fi

#? list: List all available make targets
list:
	@$(call print, "Listing commands:")
	@$(MAKE) -qp | \
		awk -F':' '/^[a-zA-Z0-9][^$$#\/\t=]*:([^=]|$$)/ {split($$1,A,/ /);for(i in A)print A[i]}' | \
		grep -v Makefile | \
		sort

#? help: List all available make targets with their descriptions
help: Makefile
	@$(call print, "Listing commands:")
	@sed -n 's/^#?//p' $< | column -t -s ':' |  sort | sed -e 's/^/ /'

#? backwards-compat-test: Run basic backwards compatibility test
backwards-compat-test:
	@$(call print, "Running backwards compatability test")
	./scripts/bw-compatibility-test/test.sh

#? sqlc: Generate sql models and queries in Go
sqlc:
	@$(call print, "Generating sql models and queries in Go")
	./scripts/gen_sqlc_docker.sh

#? sqlc-check: Make sure sql models and queries are up to date
sqlc-check: sqlc
	@$(call print, "Verifying sql code generation.")
	if test -n "$$(git status --porcelain '*.go')"; then echo "SQL models not properly generated!"; git status --porcelain '*.go'; exit 1; fi

#? rpc: Compile protobuf definitions and generate REST proxy stubs
rpc:
	@$(call print, "Compiling protos.")
	cd ./lnrpc; ./gen_protos_docker.sh

#? rpc-format: Format protobuf definition files
rpc-format:
	@$(call print, "Formatting protos.")
	cd ./lnrpc; find . -name "*.proto" | xargs clang-format --style=file -i

#? rpc-check: Make sure protobuf definitions are up to date
rpc-check: rpc
	@$(call print, "Verifying protos.")
	cd ./lnrpc; ../scripts/check-rest-annotations.sh
	if test -n "$$(git status --porcelain)"; then echo "Protos not properly formatted or not compiled with v3.4.0"; git status; git diff; exit 1; fi

#? rpc-js-compile: Compile protobuf definitions and generate JSON/WASM stubs
rpc-js-compile:
	@$(call print, "Compiling JSON/WASM stubs.")
	GOOS=js GOARCH=wasm $(GOBUILD) -tags="$(WASM_RELEASE_TAGS)" $(PKG)/lnrpc/...

#? sample-conf-check: Make sure default values in the sample-lnd.conf file are set correctly
sample-conf-check:
	@$(call print, "Checking that default values in the sample-lnd.conf file are set correctly")
	scripts/check-sample-lnd-conf.sh "$(RELEASE_TAGS)"

#? mobile-rpc: Compile mobile RPC stubs from the protobuf definitions
mobile-rpc:
	@$(call print, "Creating mobile RPC from protos.")
	cd ./lnrpc; COMPILE_MOBILE=1 SUBSERVER_PREFIX=1 ./gen_protos_docker.sh

#? vendor: Create a vendor directory with all dependencies
vendor:
	@$(call print, "Re-creating vendor directory.")
	rm -r vendor/; $(GOCC) mod vendor

#? apple: Build mobile RPC stubs and project template for iOS and macOS
apple: mobile-rpc
	@$(call print, "Building iOS and macOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=ios,iossimulator,macos -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

#? ios: Build mobile RPC stubs and project template for iOS
ios: mobile-rpc
	@$(call print, "Building iOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=ios,iossimulator -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

#? macos: Build mobile RPC stubs and project template for macOS
macos: mobile-rpc
	@$(call print, "Building macOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=macos -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

#? android: Build mobile RPC stubs and project template for Android
android: mobile-rpc
	@$(call print, "Building Android library ($(ANDROID_BUILD)).")
	mkdir -p $(ANDROID_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=android -androidapi 21 -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(ANDROID_BUILD) $(MOBILE_PKG)

#? mobile: Build mobile RPC stubs and project templates for iOS and Android
mobile: ios android

#? clean: Remove all generated files
clean:
	@$(call print, "Cleaning source.$(NC)")
	$(RM) ./lnd-debug ./lncli-debug
	$(RM) ./lnd-itest ./lncli-itest
	$(RM) -r ./vendor .vendor-new

#? clean-mobile: Remove all generated mobile files
clean-mobile:
	@$(call print, "Cleaning autogenerated mobile RPC stubs.")
	$(RM) -r mobile/build
	$(RM) mobile/*_generated.go

#? clean-docker-volumes: Remove Docker cache volumes used for local development
clean-docker-volumes:
	@$(call print, "Removing Docker cache volumes.")
	docker volume rm lnd-go-build-cache lnd-go-mod-cache lnd-go-lint-cache 2>/dev/null || true

.PHONY: all \
	btcd \
	default \
	build \
	install \
	scratch \
	check \
	help \
	itest-only \
	itest \
	unit \
	unit-debug \
	unit-cover \
	unit-race \
	flakehunter \
	flake-unit \
	fmt \
	lint \
	list \
	rpc \
	rpc-format \
	rpc-check \
	rpc-js-compile \
	mobile-rpc \
	vendor \
	ios \
	android \
	mobile \
	clean \
	clean-docker-volumes
