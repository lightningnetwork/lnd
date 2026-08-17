//go:build walletrpc
// +build walletrpc

package walletrpc

import (
	"testing"

	"github.com/lightningnetwork/lnd/build"
	"github.com/stretchr/testify/require"
)

// TestXCreateAccountExperimentalGate asserts the acknowledgement gate, which is
// what keeps an operator from creating an unrecoverable account by accident.
//
// The gate has to admit two callers: a dev build, and a release build whose
// caller has explicitly attested. Getting this wrong in the strict direction
// makes the RPC unusable on the release images real deployments run; getting
// it wrong in the loose direction silently drops the warning entirely.
func TestXCreateAccountExperimentalGate(t *testing.T) {
	t.Parallel()

	if build.IsDevBuild() {
		// A dev build lets the call through, so it would reach the
		// wallet and there is nothing to assert here without one. That
		// direction is covered by the itest, whose binaries are built
		// with this tag.
		t.Skip("gate is open on dev builds; covered by the itest")
	}

	// The wallet is never reached when the gate refuses, so a nil config
	// is enough to prove the refusal happens first.
	w := &WalletKit{}

	_, err := w.XCreateAccount(t.Context(), &XCreateAccountRequest{
		Name:        "custom",
		AddressType: AddressType_TAPROOT_PUBKEY,
	})
	require.ErrorIs(t, err, errAccountCreationNotAcked)

	// The same request with the acknowledgement set gets past the gate,
	// which on a nil wallet means it panics rather than returning the
	// refusal — so assert on the gate, not on what follows it.
	require.Panics(t, func() {
		//nolint:errcheck // Panics past the gate, which is the point.
		w.XCreateAccount(t.Context(), &XCreateAccountRequest{
			Name:              "custom",
			AddressType:       AddressType_TAPROOT_PUBKEY,
			IKnowWhatIAmDoing: true,
		})
	}, "acknowledged request must get past the gate")
}
