PKG := github.com/lightningnetwork/lnd
ESCPKG := github.com\/lightningnetwork\/lnd

BTCD_PKG := github.com/btcsuite/btcd
GLIDE_PKG := github.com/Masterminds/glide
GOVERALLS_PKG := github.com/mattn/goveralls
LINT_PKG := gopkg.in/alecthomas/gometalinter.v2

GO_BIN := ${GOPATH}/bin
BTCD_BIN := $(GO_BIN)/btcd
GLIDE_BIN := $(GO_BIN)/glide
GOVERALLS_BIN := $(GO_BIN)/goveralls
LINT_BIN := $(GO_BIN)/gometalinter.v2

HAVE_BTCD := $(shell command -v $(BTCD_BIN) 2> /dev/null)
HAVE_GLIDE := $(shell command -v $(GLIDE_BIN) 2> /dev/null)
HAVE_GOVERALLS := $(shell command -v $(GOVERALLS_BIN) 2> /dev/null)
HAVE_LINTER := $(shell command -v $(LINT_BIN) 2> /dev/null)

BTCD_DIR :=${GOPATH}/src/$(BTCD_PKG)

COMMIT := $(shell git describe --abbrev=40 --dirty)
LDFLAGS := -ldflags "-X $(PKG)/build.Commit=$(COMMIT)"

GLIDE_COMMIT := 84607742b10f492430762d038e954236bbaf23f7
BTCD_COMMIT := $(shell cat go.sum | \
		grep $(BTCD_PKG) | \
		tail -n1 | \
		awk -F " " '{ print $$2 }' | \
		awk -F "-" '{ print $$3 }' | \
		awk -F "/" '{ print $$1 }')

GOBUILD := GO111MODULE=on go build -v
GOINSTALL := GO111MODULE=on go install -v
GOTEST := go test -v

GOLIST := go list $(PKG)/... | grep -v '/vendor/'
GOLISTCOVER := $(shell go list -f '{{.ImportPath}}' ./... | sed -e 's/^$(ESCPKG)/./')
GOLISTLINT := $(shell go list -f '{{.Dir}}' ./... | grep -v 'lnrpc')

RM := rm -f
CP := cp
MAKE := make
XARGS := xargs -L 1

include make/testing_flags.mk

DEV_TAGS := $(if ${tags},$(DEV_TAGS) ${tags},$(DEV_TAGS))
PROD_TAGS := $(if ${tags},$(PROD_TAGS) ${tags},$(PROD_TAGS))

COVER = for dir in $(GOLISTCOVER); do \
		$(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" \
			-covermode=count \
			-coverprofile=$$dir/profile.tmp $$dir; \
		\
		if [ $$? != 0 ] ; \
		then \
			exit 1; \
		fi ; \
		\
		if [ -f $$dir/profile.tmp ]; then \
			cat $$dir/profile.tmp | \
				tail -n +2 >> profile.cov; \
			$(RM) $$dir/profile.tmp; \
		fi \
	done

LINT = $(LINT_BIN) \
	--disable-all \
	--enable=gofmt \
	--enable=vet \
	--enable=golint \
	--line-length=72 \
	--deadline=4m $(GOLISTLINT) 2>&1 | \
	grep -v 'ALL_CAPS\|OP_' 2>&1 | \
	tee /dev/stderr

CGO_STATUS_QUO := ${CGO_ENABLED}

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

$(GLIDE_BIN):
	@$(call print, "Fetching glide.")
	go get -d $(GLIDE_PKG)
	cd ${GOPATH}/src/$(GLIDE_PKG) && ( git checkout $(GLIDE_COMMIT) || ( git fetch --all && git checkout $(GLIDE_COMMIT) ) )
	$(GOINSTALL) $(GLIDE_PKG)

$(GOVERALLS_BIN):
	@$(call print, "Fetching goveralls.")
	go get -u $(GOVERALLS_PKG)

$(LINT_BIN):
	@$(call print, "Fetching gometalinter.v2")
	go get -u $(LINT_PKG)

$(BTCD_DIR):
	@$(call print, "Fetching btcd.")
	go get -d github.com/btcsuite/btcd

btcd: $(GLIDE_BIN) $(BTCD_DIR)
	@$(call print, "Compiling btcd dependencies.")
	cd $(BTCD_DIR) && ( git checkout $(BTCD_COMMIT) || ( git fetch --all && git checkout $(BTCD_COMMIT) ) ) && glide install
	@$(call print, "Installing btcd and btcctl.")
	$(GOINSTALL) $(BTCD_PKG)
	$(GOINSTALL) $(BTCD_PKG)/cmd/btcctl

# ============
# INSTALLATION
# ============

build:
	@$(call print, "Building debug lnd and lncli.")
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lnd-debug $(LDFLAGS) $(PKG)
	$(GOBUILD) -tags="$(DEV_TAGS)" -o lncli-debug $(LDFLAGS) $(PKG)/cmd/lncli

install:
	@$(call print, "Installing lnd and lncli.")
	$(GOINSTALL) -tags="$(PROD_TAGS)" $(LDFLAGS) $(PKG)
	$(GOINSTALL) -tags="$(PROD_TAGS)" $(LDFLAGS) $(PKG)/cmd/lncli

scratch: build


# =======
# TESTING
# =======

check: unit itest

itest-only: 
	@$(call print, "Running integration tests.")
	$(ITEST)

itest: btcd build itest-only

unit: btcd
	@$(call print, "Running unit tests.")
	$(UNIT)

unit-cover:
	@$(call print, "Running unit coverage tests.")
	echo "mode: count" > profile.cov
	$(COVER)
		
unit-race:
	@$(call print, "Running unit race tests.")
	export CGO_ENABLED=1; env GORACE="history_size=7 halt_on_errors=1" $(UNIT_RACE)
	export CGO_ENABLED=$(CGO_STATUS_QUO)

goveralls: $(GOVERALLS_BIN)
	@$(call print, "Sending coverage report.")
	$(GOVERALLS_BIN) -coverprofile=profile.cov -service=travis-ci

# =============
# FLAKE HUNTING
# =============

flakehunter: build
	@$(call print, "Flake hunting integration tests.")
	while [ $$? -eq 0 ]; do $(ITEST); done

flake-unit:
	@$(call print, "Flake hunting unit tests.")
	GOTRACEBACK=all $(UNIT) -count=1
	while [ $$? -eq 0 ]; do /bin/sh -c "GOTRACEBACK=all $(UNIT) -count=1"; done

# =========
# UTILITIES
# =========

fmt:
	@$(call print, "Formatting source.")
	$(GOLIST) | $(XARGS) go fmt -x

lint: $(LINT_BIN)
	@$(call print, "Linting source.")
	$(LINT_BIN) --install 1> /dev/null
	test -z "$$($(LINT))"

list:
	@$(call print, "Listing commands.")
	@$(MAKE) -qp | \
		awk -F':' '/^[a-zA-Z0-9][^$$#\/\t=]*:([^=]|$$)/ {split($$1,A,/ /);for(i in A)print A[i]}' | \
		grep -v Makefile | \
		sort

rpc:
	@$(call print, "Compiling protos.")
	cd ./lnrpc; ./gen_protos.sh

clean:
	@$(call print, "Cleaning source.$(NC)")
	$(RM) ./lnd-debug ./lncli-debug
	$(RM) -r ./vendor .vendor-new


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
	unit-cover \
	unit-race \
	goveralls \
	flakehunter \
	flake-unit \
	fmt \
	lint \
	list \
	rpc \
	clean
