package chanstate

import (
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

var _ FwdPackageStore = (*KVStore)(nil)

// AckSettleFailRefs atomically updates the settle-fail filters in the
// referenced forwarding packages.
func (s *KVStore) AckSettleFailRefs(
	settleFailRefs ...SettleFailRef) error {

	return kvdb.Batch(s.backend, func(tx kvdb.RwTx) error {
		return ackSettleFails(tx, settleFailRefs)
	})
}

// LoadChannelFwdPkgs loads all forwarding packages for a channel.
func (s *KVStore) LoadChannelFwdPkgs(
	source lnwire.ShortChannelID) ([]*FwdPkg, error) {

	var fwdPkgs []*FwdPkg
	err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		var err error
		fwdPkgs, err = loadChannelFwdPkgs(tx, source)

		return err
	}, func() {
		fwdPkgs = nil
	})
	if err != nil {
		return nil, err
	}

	return fwdPkgs, nil
}
