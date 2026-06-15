package lncfg_test

import (
	"testing"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/stretchr/testify/require"
)

// TestRemoteSignerValidateInboundRequiresListeners makes sure inbound remote
// signer mode still requires at least one dedicated listener address.
func TestRemoteSignerValidateInboundRequiresListeners(t *testing.T) {
	cfg := lncfg.DefaultRemoteSignerCfg()
	cfg.Enable = true
	cfg.AllowInboundConnection = true

	err := cfg.Validate()
	require.ErrorContains(t, err, "remotesigner.rpclisten must be set")

	cfg.RPCListeners = []string{"localhost"}

	require.NoError(t, cfg.Validate())
}
