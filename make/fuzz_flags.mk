FUZZPKG = brontide lnwire watchtower/wtwire zpay32
FUZZ_TEST_RUN_TIME = 30s
FUZZ_NUM_PROCESSES = 4

# If specific package is being fuzzed, construct the full name of the
# subpackage.
ifneq ($(pkg),)
FUZZPKG := $(pkg)
endif

# The default run time per fuzz test is pretty low and normally will be
# overwritten by a user depending on the time they have available.
ifneq ($(fuzztime),)
FUZZ_TEST_RUN_TIME := $(fuzztime)
endif

# Overwrites the number of parallel processes. Should be set to the number of
# processor cores in a system.
ifneq ($(parallel),)
FUZZ_NUM_PROCESSES := $(parallel)
endif
