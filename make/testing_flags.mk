DEV_TAGS = dev
LOG_TAGS =
TEST_FLAGS =
COVER_PKG = $$(go list ./... | grep -v lnrpc)

# If specific package is being unit tested, construct the full name of the
# subpackage.
ifneq ($(pkg),)
UNITPKG := $(PKG)/$(pkg)
UNIT_TARGETED = yes
COVER_PKG = $(PKG)/$(pkg)
endif

# If a specific unit test case is being target, construct test.run filter.
ifneq ($(case),)
TEST_FLAGS += -test.run=$(case)
UNIT_TARGETED = yes
endif

# Define the integration test.run filter if the icase argument was provided.
ifneq ($(icase),)
TEST_FLAGS += -test.run=TestLightningNetworkDaemon/$(icase)
endif

# Define the log tags that will be applied only when running unit tests. If none
# are provided, we default to "nolog" which will be silent.
ifneq ($(log),)
LOG_TAGS := ${log}
else
LOG_TAGS := nolog
endif

# If a timeout was requested, construct initialize the proper flag for the go
# test command. If not, we set 20m (up from the default 10m).
ifneq ($(timeout),)
TEST_FLAGS += -test.timeout=$(timeout)
else
TEST_FLAGS += -test.timeout=40m
endif

# UNIT_TARGTED is undefined iff a specific package and/or unit test case is
# not being targeted.
UNIT_TARGETED ?= no

# If a specific package/test case was requested, run the unit test for the
# targeted case. Otherwise, default to running all tests.
ifeq ($(UNIT_TARGETED), yes)
UNIT := $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) $(UNITPKG)
UNIT_RACE := $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS) -race $(UNITPKG)
endif

ifeq ($(UNIT_TARGETED), no)
UNIT := $(GOLIST) | $(XARGS) env $(GOTEST) -tags="$(DEV_TAGS) $(LOG_TAGS)" $(TEST_FLAGS)
UNIT_RACE := $(UNIT) -race
endif


# Construct the integration test command with the added build flags.
ITEST_TAGS := $(DEV_TAGS) rpctest chainrpc walletrpc signrpc invoicesrpc autopilotrpc routerrpc watchtowerrpc

# Default to btcd backend if not set.
ifneq ($(backend),)
ITEST_TAGS += ${backend}
else
ITEST_TAGS += btcd
endif

ITEST := rm lntest/itest/*.log; date; $(GOTEST) ./lntest/itest -tags="$(ITEST_TAGS)" $(TEST_FLAGS) -logoutput -goroutinedump
