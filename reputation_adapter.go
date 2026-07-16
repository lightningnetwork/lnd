package lnd

import (
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/reputation"
)

// reputationManagerAdapter bridges the reputation.Manager to the switch's
// read-only htlcswitch.ReputationManager seam. The manager's hook signatures
// already match the switch interface (both use models.CircuitKey and
// lnwire.MilliSatoshi), so this is a thin, explicit bridge that keeps the
// switch from importing the reputation package directly.
type reputationManagerAdapter struct {
	*reputation.Manager
}

// Compile-time assertion that the adapter satisfies the switch's read-only
// reputation seam.
var _ htlcswitch.ReputationManager = (*reputationManagerAdapter)(nil)

// newReputationManagerAdapter wraps a reputation.Manager as an
// htlcswitch.ReputationManager.
func newReputationManagerAdapter(
	m *reputation.Manager) htlcswitch.ReputationManager {

	return &reputationManagerAdapter{Manager: m}
}
