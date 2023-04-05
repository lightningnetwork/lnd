PKG := github.com/lightningnetwork/lnd
ESCPKG := github.com\/lightningnetwork\/lnd
MOBILE_PKG := $(PKG)/mobile
TOOLS_DIR := tools

BTCD_PKG := github.com/btcsuite/btcd
GOACC_PKG := github.com/ory/go-acc
GOIMPORTS_PKG := github.com/rinchsan/gosimports/cmd/gosimports

GO_BIN := ${GOPATH}/bin
BTCD_BIN := $(GO_BIN)/btcd
GOIMPORTS_BIN := $(GO_BIN)/gosimports
GOMOBILE_BIN := $(GO_BIN)/gomobile
GOACC_BIN := $(GO_BIN)/go-acc

MOBILE_BUILD_DIR :=${GOPATH}/src/$(MOBILE_PKG)/build
IOS_BUILD_DIR := $(MOBILE_BUILD_DIR)/ios
IOS_BUILD := $(IOS_BUILD_DIR)/Lndmobile.xcframework
ANDROID_BUILD_DIR := $(MOBILE_BUILD_DIR)/android
ANDROID_BUILD := $(ANDROID_BUILD_DIR)/Lndmobile.aar

COMMIT := $(shell git describe --tags --dirty)

GOBUILD := go build -v
GOINSTALL := go install -v
GOTEST := go test

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

DOCKER_TOOLS = docker run -v $$(pwd):/build lnd-tools

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
$(GOACC_BIN):
	@$(call print, "Installing go-acc.")
	cd $(TOOLS_DIR); go install -trimpath -tags=tools $(GOACC_PKG)

$(BTCD_BIN):
	@$(call print, "Installing btcd.")
	cd $(TOOLS_DIR); go install -trimpath $(BTCD_PKG)

$(GOIMPORTS_BIN):
	@$(call print, "Installing goimports.")
	cd $(TOOLS_DIR); go install -trimpath $(GOIMPORTS_PKG)

# ============
# INSTALLATION
# ============

build:
	@$(call print, "Building debug lnd and lncli.")
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lnd-debug $(DEV_GCFLAGS) $(DEV_LDFLAGS) $(PKG)/cmd/lnd
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lncli-debug $(DEV_GCFLAGS) $(DEV_LDFLAGS) $(PKG)/cmd/lncli

build-itest:
	@$(call print, "Building itest btcd and lnd.")
	CGO_ENABLED=0 $(GOBUILD) -tags="integration" -o itest/btcd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(BTCD_PKG)
	CGO_ENABLED=0 $(GOBUILD) -tags="$(ITEST_TAGS)" -o itest/lnd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(PKG)/cmd/lnd

	@$(call print, "Building itest binary for ${backend} backend.")
	CGO_ENABLED=0 $(GOTEST) -v ./itest -tags="$(DEV_TAGS) $(RPC_TAGS) integration $(backend)" -c -o itest/itest.test$(EXEC_SUFFIX)

build-itest-race:
	@$(call print, "Building itest btcd and lnd with race detector.")
	CGO_ENABLED=0 $(GOBUILD) -tags="integration" -o itest/btcd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(BTCD_PKG)
	CGO_ENABLED=1 $(GOBUILD) -race -tags="$(ITEST_TAGS)" -o itest/lnd-itest$(EXEC_SUFFIX) $(DEV_LDFLAGS) $(PKG)/cmd/lnd

	@$(call print, "Building itest binary for ${backend} backend.")
	CGO_ENABLED=0 $(GOTEST) -v ./itest -tags="$(DEV_TAGS) $(RPC_TAGS) integration $(backend)" -c -o itest/itest.test$(EXEC_SUFFIX)

install:
	@$(call print, "Installing lnd and lncli.")
	$(GOINSTALL) -tags="${tags}" -ldflags="$(RELEASE_LDFLAGS)" $(PKG)/cmd/lnd
	$(GOINSTALL) -tags="${tags}" -ldflags="$(RELEASE_LDFLAGS)" $(PKG)/cmd/lncli

release-install:
	@$(call print, "Installing release lnd and lncli.")
	env CGO_ENABLED=0 $(GOINSTALL) -v -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" $(PKG)/cmd/lnd
	env CGO_ENABLED=0 $(GOINSTALL) -v -trimpath -ldflags="$(RELEASE_LDFLAGS)" -tags="$(RELEASE_TAGS)" $(PKG)/cmd/lncli

# Make sure the generated mobile RPC stubs don't influence our vendor package
# by removing them first in the clean-mobile target.
release: clean-mobile
	@$(call print, "Releasing lnd and lncli binaries.")
	$(VERSION_CHECK)
	./scripts/release.sh build-release "$(VERSION_TAG)" "$(BUILD_SYSTEM)" "$(RELEASE_TAGS)" "$(RELEASE_LDFLAGS)"

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

check: unit itest

db-instance:
ifeq ($(dbbackend),postgres)
	# Remove a previous postgres instance if it exists.
	docker rm lnd-postgres --force || echo "Starting new postgres container"

	# Start a fresh postgres instance. Allow a maximum of 500 connections so
	# that multiple lnd instances with a maximum number of connections of 50
	# each can run concurrently.
	docker run --name lnd-postgres -e POSTGRES_PASSWORD=postgres -p 6432:5432 -d postgres:13-alpine -N 500
	docker logs -f lnd-postgres &

	# Wait for the instance to be started.
	sleep $(POSTGRES_START_DELAY)
endif

itest-only: db-instance
	@$(call print, "Running integration tests with ${backend} backend.")
	rm -rf itest/*.log itest/.logs-*; date
	EXEC_SUFFIX=$(EXEC_SUFFIX) scripts/itest_part.sh 0 1 $(TEST_FLAGS) $(ITEST_FLAGS)

itest: build-itest itest-only

itest-race: build-itest-race itest-only

itest-parallel: build-itest db-instance
	@$(call print, "Running tests")
	rm -rf itest/*.log itest/.logs-*; date
	EXEC_SUFFIX=$(EXEC_SUFFIX) echo "$$(seq 0 $$(expr $(ITEST_PARALLELISM) - 1))" | xargs -P $(ITEST_PARALLELISM) -n 1 -I {} scripts/itest_part.sh {} $(NUM_ITEST_TRANCHES) $(TEST_FLAGS) $(ITEST_FLAGS)

itest-clean:
	@$(call print, "Cleaning old itest processes")
	killall lnd-itest || echo "no running lnd-itest process found";

unit: $(BTCD_BIN)
	@$(call print, "Running unit tests.")
	$(UNIT)

unit-debug: $(BTCD_BIN)
	@$(call print, "Running debug unit tests.")
	$(UNIT_DEBUG)

unit-cover: $(GOACC_BIN)
	@$(call print, "Running unit coverage tests.")
	$(GOACC)

unit-race:
	@$(call print, "Running unit race tests.")
	env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)

# =============
# FLAKE HUNTING
# =============

flakehunter: build-itest
	@$(call print, "Flake hunting ${backend} integration tests.")
	while [ $$? -eq 0 ]; do make itest-only icase='${icase}' backend='${backend}'; done

flake-unit:
	@$(call print, "Flake hunting unit tests.")
	while [ $$? -eq 0 ]; do GOTRACEBACK=all $(UNIT) -count=1; done

flakehunter-parallel:
	@$(call print, "Flake hunting ${backend} integration tests in parallel.")
	while [ $$? -eq 0 ]; do make itest-parallel tranches=1 parallel=${ITEST_PARALLELISM} icase='${icase}' backend='${backend}'; done

# =============
# FUZZING
# =============

fuzz:
	@$(call print, "Fuzzing packages '$(FUZZPKG)'.")
	scripts/fuzz.sh run "$(FUZZPKG)" "$(FUZZ_TEST_RUN_TIME)" "$(FUZZ_NUM_PROCESSES)"

# =========
# UTILITIES
# =========

fmt: $(GOIMPORTS_BIN)
	@$(call print, "Fixing imports.")
	gosimports -w $(GOFILES_NOVENDOR)
	@$(call print, "Formatting source.")
	gofmt -l -w -s $(GOFILES_NOVENDOR)

fmt-check: fmt
	@$(call print, "Checking fmt results.")
	if test -n "$$(git status --porcelain)"; then echo "code not formatted correctly, please run `make fmt` again!"; git status; git diff; exit 1; fi

lint: docker-tools
	@$(call print, "Linting source.")
	$(DOCKER_TOOLS) golangci-lint run -v $(LINT_WORKERS)

list:
	@$(call print, "Listing commands.")
	@$(MAKE) -qp | \
		awk -F':' '/^[a-zA-Z0-9][^$$#\/\t=]*:([^=]|$$)/ {split($$1,A,/ /);for(i in A)print A[i]}' | \
		grep -v Makefile | \
		sort

rpc:
	@$(call print, "Compiling protos.")
	cd ./lnrpc; ./gen_protos_docker.sh

rpc-format:
	@$(call print, "Formatting protos.")
	cd ./lnrpc; find . -name "*.proto" | xargs clang-format --style=file -i

rpc-check: rpc
	@$(call print, "Verifying protos.")
	cd ./lnrpc; ../scripts/check-rest-annotations.sh
	if test -n "$$(git status --porcelain)"; then echo "Protos not properly formatted or not compiled with v3.4.0"; git status; git diff; exit 1; fi

rpc-js-compile:
	@$(call print, "Compiling JSON/WASM stubs.")
	GOOS=js GOARCH=wasm $(GOBUILD) -tags="$(WASM_RELEASE_TAGS)" $(PKG)/lnrpc/...

sample-conf-check:
	@$(call print, "Making sure every flag has an example in the sample-lnd.conf file")
	for flag in $$(GO_FLAGS_COMPLETION=1 go run -tags="$(RELEASE_TAGS)" $(PKG)/cmd/lnd -- | grep -v help | cut -c3-); do if ! grep -q $$flag sample-lnd.conf; then echo "Command line flag --$$flag not added to sample-lnd.conf"; exit 1; fi; done

mobile-rpc:
	@$(call print, "Creating mobile RPC from protos.")
	cd ./lnrpc; COMPILE_MOBILE=1 SUBSERVER_PREFIX=1 ./gen_protos_docker.sh

vendor:
	@$(call print, "Re-creating vendor directory.")
	rm -r vendor/; go mod vendor

apple: mobile-rpc
	@$(call print, "Building iOS and macOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=ios,iossimulator,macos -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

ios: mobile-rpc
	@$(call print, "Building iOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=ios,iossimulator -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

macos: mobile-rpc
	@$(call print, "Building macOS cxframework ($(IOS_BUILD)).")
	mkdir -p $(IOS_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=macos -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(IOS_BUILD) $(MOBILE_PKG)

android: mobile-rpc
	@$(call print, "Building Android library ($(ANDROID_BUILD)).")
	mkdir -p $(ANDROID_BUILD_DIR)
	$(GOMOBILE_BIN) bind -target=android -androidapi 21 -tags="mobile $(DEV_TAGS) $(RPC_TAGS)" -ldflags "$(RELEASE_LDFLAGS)" -v -o $(ANDROID_BUILD) $(MOBILE_PKG)

mobile: ios android

clean:
	@$(call print, "Cleaning source.$(NC)")
	$(RM) ./lnd-debug ./lncli-debug
	$(RM) ./lnd-itest ./lncli-itest
	$(RM) -r ./vendor .vendor-new

clean-mobile:
	@$(call print, "Cleaning autogenerated mobile RPC stubs.")
	$(RM) -r mobile/build
	$(RM) mobile/*_generated.go

.PHONY: all \
	btcd \
	default \
	build \
	install \
	scratch \
	check \
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
	clean
