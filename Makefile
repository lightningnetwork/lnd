PKG := github.com/lightningnetwork/lnd
HAVE_DEP := $(shell command -v dep 2> /dev/null)
COMMIT := $(shell git rev-parse HEAD)
LDFLAGS := -ldflags "-X main.Commit=$(COMMIT)"
RM := rm

all: scratch


# ==============================================================================
# INSTALLATION
# ==============================================================================

deps:
ifndef HAVE_DEP
	@echo "Fetching dep."
	go get -u github.com/golang/dep/cmd/dep
endif
	@echo "Building dependencies."
	dep ensure -v

build: 
	@echo "Building lnd and lncli."
	go build -v -o lnd $(LDFLAGS) $(PKG)
	go build -v -o lncli $(LDFLAGS) $(PKG)/cmd/lncli

install:
	@echo "Installing lnd and lncli."
	go install -v $(LDFLAGS) $(PKG)
	go install -v $(LDFLAGS) $(PKG)/cmd/lncli

scratch: deps build


# ==============================================================================
# TESTING
# ==============================================================================

# Define the integration test.run filter if the icase argument was provided.
ifneq ($(icase),)
	ITESTCASE := -test.run=TestLightningNetworkDaemon/$(icase)
endif

# UNIT_TARGTED is defined iff a specific package and/or unit test case is being
# targeted.
UNIT_TARGETED = 

# If specific package is being unit tested, construct the full name of the
# subpackage.
ifneq ($(pkg),)
	UNITPKG := $(PKG)/$(pkg)
	UNIT_TARGETED = yes
endif

# If a specific unit test case is being target, construct test.run filter.
ifneq ($(case),)
	UNITCASE := -test.run=$(case)
	UNIT_TARGETED = yes
endif

# If no unit targeting was input, default to running all tests. Otherwise,
# construct the command to run the specific package/test case.
ifndef UNIT_TARGETED
	UNIT := go list $(PKG)/... | grep -v '/vendor/' | xargs go test
else
	UNIT := go test -test.v $(UNITPKG) $(UNITCASE)
endif

check: unit itest

itest: install
	@echo "Running integration tests."
	go test -v -tags rpctest -logoutput $(ITESTCASE)

unit:
	@echo "Running unit tests."
	$(UNIT)


# ==============================================================================
# UTILITIES
# ==============================================================================

fmt:
	go list $(PKG)/... | grep -v '/vendor/' | xargs go fmt -x

clean:
	$(RM) ./lnd ./lncli
	$(RM) -rf vendor


.PHONY: all deps build install scratch check itest unit fmt clean
