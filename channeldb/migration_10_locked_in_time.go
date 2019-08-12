package channeldb

import (
	"time"

	"github.com/coreos/bbolt"
)

func migrateLockedInTime(tx *bbolt.Tx) error {
	log.Infof("Migrating forwarding packages locked-in time")

	fwdPkgBkt := tx.Bucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return nil
	}

	if err := fwdPkgBkt.ForEach(func(sourceKey, _ []byte) error {
		sourceBkt := fwdPkgBkt.Bucket(sourceKey[:])
		if sourceBkt == nil {
			return ErrCorruptedFwdPkg
		}

		return sourceBkt.ForEach(func(heightKey, _ []byte) error {
			heightBkt := sourceBkt.Bucket(heightKey[:])
			if heightBkt == nil {
				return ErrCorruptedFwdPkg
			}

			lockedInTime := LockedInTime{
				// Set accepted block height to zero. This is
				// obviously not the real locked-in time, but
				// this way any accepted htlcs for potential
				// hodl invoices won't be canceled back.
				BlockHeight: 0,

				// Set timestamp to current time. This is not
				// the real locked-in time, but it is our best
				// guess.
				Timestamp: time.Now(),
			}

			return putLockedInTime(heightBkt, lockedInTime)
		})
	}); err != nil {
		return err
	}

	log.Infof("Migration of forwarding packages locked-in time completed!")

	return nil
}
