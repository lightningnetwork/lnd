package lnd

import (
	"testing"

	"github.com/lightningnetwork/lnd/trampolinerelay"
	"github.com/stretchr/testify/require"
)

// TestTrampolineRegistryDisabled prevents a typed nil service from enabling
// trampoline processing or receiving link shutdown callbacks.
func TestTrampolineRegistryDisabled(t *testing.T) {
	t.Parallel()

	s := &server{}
	require.True(t, s.trampolineRegistry() == nil)

	s.trampolineRelay = &trampolinerelay.Service{}
	require.Same(t, s.trampolineRelay, s.trampolineRegistry())
}
