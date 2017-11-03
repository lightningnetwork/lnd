/*
Package lntest provides testing utilities for the lnd repository.

This package contains infrastructure for integration tests that launch full lnd
nodes in a controlled environment and interact with them via RPC. Using a
NetworkHarness, a test can launch multiple lnd nodes, open channels between
them, create defined network topologies, and anything else that is possible with
RPC commands.
*/
package lntest
