package chanstate

import "github.com/lightningnetwork/lnd/lnwire"

// FwdPackageStore provides backend-independent access to active-channel
// forwarding packages.
type FwdPackageStore interface {
	// LoadChannelFwdPkgs loads all forwarding packages for the given
	// channel.
	LoadChannelFwdPkgs(source lnwire.ShortChannelID) ([]*FwdPkg, error)

	// AckSettleFailRefs atomically updates the settle-fail filters in the
	// forwarding packages referenced by settleFailRefs.
	AckSettleFailRefs(settleFailRefs ...SettleFailRef) error
}
