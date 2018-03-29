TEST_FLAGS =

# If specific package is being unit tested, construct the full name of the
# subpackage.
ifneq ($(pkg),)
UNITPKG := $(PKG)/$(pkg)
UNIT_TARGETED = yes
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

# If a timeout was requested, construct initialize the proper flag for the go
# test command.
ifneq ($(timeout),)
TEST_FLAGS += -test.timeout=$(timeout)
endif

# UNIT_TARGTED is undefined iff a specific package and/or unit test case is
# not being targeted.
UNIT_TARGETED ?= no

# If a specific package/test case was requested, run the unit test for the
# targeted case. Otherwise, default to running all tests.
ifeq ($(UNIT_TARGETED), yes)
UNIT := $(GOTEST) $(TEST_FLAGS) $(UNITPKG)
UNIT_RACE := $(GOTEST) $(TEST_FLAGS) -race $(UNITPKG)
endif

ifeq ($(UNIT_TARGETED), no)
UNIT := $(GOLIST) | $(XARGS) $(GOTEST) $(TEST_FLAGS) 
UNIT_RACE := $(UNIT) -race 
endif


# Construct the integration test command with the added build flags.
ITEST := $(GOTEST) $(TEST_FLAGS) -tags rpctest -logoutput
