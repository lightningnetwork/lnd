package migration21

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migration21/common"
	"github.com/lightningnetwork/lnd/channeldb/migration21/current"
	"github.com/lightningnetwork/lnd/channeldb/migration21/legacy"
)

var (
	byteOrder = binary.BigEndian

	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Within
	// that node ID bucket, all attributes required to track, update, and
	// close a channel are stored.
	//
	// openChan -> nodeID -> chanPoint
	//
	// TODO(roasbeef): flesh out comment
	openChannelBucket = []byte("open-chan-bucket")

	// commitDiffKey stores the current pending commitment state we've
	// extended to the remote party (if any). Each time we propose a new
	// state, we store the information necessary to reconstruct this state
	// from the prior commitment. This allows us to resync the remote party
	// to their expected state in the case of message loss.
	//
	// TODO(roasbeef): rename to commit chain?
	commitDiffKey = []byte("commit-diff-key")

	// unsignedAckedUpdatesKey is an entry in the channel bucket that
	// contains the remote updates that we have acked, but not yet signed
	// for in one of our remote commits.
	unsignedAckedUpdatesKey = []byte("unsigned-acked-updates-key")

	// remoteUnsignedLocalUpdatesKey is an entry in the channel bucket that
	// contains the local updates that the remote party has acked, but
	// has not yet signed for in one of their local commits.
	remoteUnsignedLocalUpdatesKey = []byte("remote-unsigned-local-updates-key")

	// networkResultStoreBucketKey is used for the root level bucket that
	// stores the network result for each payment ID.
	networkResultStoreBucketKey = []byte("network-result-store-bucket")

	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("closed-chan-bucket")

	// fwdPackagesKey is the root-level bucket that all forwarding packages
	// are written. This bucket is further subdivided based on the short
	// channel ID of each channel.
	fwdPackagesKey = []byte("fwd-packages")
)

// MigrateDatabaseWireMessages performs a migration in all areas that we
// currently store wire messages without length prefixes. This includes the
// CommitDiff struct, ChannelCloseSummary, LogUpdates, and also the
// networkResult struct as well.
func MigrateDatabaseWireMessages(tx kvdb.RwTx) error {
	// The migration will proceed in three phases: we'll need to update any
	// pending commit diffs, then any unsigned acked updates for all open
	// channels, then finally we'll need to update all the current
	// stored network results for payments in the switch.
	//
	// In this phase, we'll migrate the open channel data.
	if err := migrateOpenChanBucket(tx); err != nil {
		return err
	}

	// Next, we'll update all the present close channel summaries as well.
	if err := migrateCloseChanSummaries(tx); err != nil {
		return err
	}

	// We'll migrate forwarding packages, which have log updates as part of
	// their serialized data.
	if err := migrateForwardingPackages(tx); err != nil {
		return err
	}

	// Finally, we'll update the pending network results as well.
	return migrateNetworkResults(tx)
}

func migrateOpenChanBucket(tx kvdb.RwTx) error {
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)

	// If no bucket is found, we can exit early.
	if openChanBucket == nil {
		return nil
	}

	type channelPath struct {
		nodePub   []byte
		chainHash []byte
		chanPoint []byte
	}
	var channelPaths []channelPath
	err := openChanBucket.ForEach(func(nodePub, v []byte) error {
		// Ensure that this is a key the same size as a pubkey, and
		// also that it leads directly to a bucket.
		if len(nodePub) != 33 || v != nil {
			return nil
		}

		nodeChanBucket := openChanBucket.NestedReadBucket(nodePub)
		if nodeChanBucket == nil {
			return fmt.Errorf("no bucket for node %x", nodePub)
		}

		// The next layer down is all the chains that this node
		// has channels on with us.
		return nodeChanBucket.ForEach(func(chainHash, v []byte) error {
			// If there's a value, it's not a bucket so
			// ignore it.
			if v != nil {
				return nil
			}

			chainBucket := nodeChanBucket.NestedReadBucket(
				chainHash,
			)
			if chainBucket == nil {
				return fmt.Errorf("unable to read "+
					"bucket for chain=%x", chainHash)
			}

			return chainBucket.ForEach(func(chanPoint, v []byte) error {
				// If there's a value, it's not a bucket so
				// ignore it.
				if v != nil {
					return nil
				}

				channelPaths = append(channelPaths, channelPath{
					nodePub:   nodePub,
					chainHash: chainHash,
					chanPoint: chanPoint,
				})

				return nil
			})
		})
	})
	if err != nil {
		return err
	}

	// Now that we have all the paths of the channel we need to migrate,
	// we'll update all the state in a distinct step to avoid weird
	// behavior from  modifying buckets in a ForEach statement.
	for _, channelPath := range channelPaths {
		// First, we'll extract it from the node's chain bucket.
		nodeChanBucket := openChanBucket.NestedReadWriteBucket(
			channelPath.nodePub,
		)
		chainBucket := nodeChanBucket.NestedReadWriteBucket(
			channelPath.chainHash,
		)
		chanBucket := chainBucket.NestedReadWriteBucket(
			channelPath.chanPoint,
		)

		// At this point, we have the channel bucket now, so we'll
		// check to see if this channel has a pending commitment or
		// not.
		commitDiffBytes := chanBucket.Get(commitDiffKey)
		if commitDiffBytes != nil {
			// Now that we have the commit diff in the _old_
			// encoding, we'll write it back to disk using the new
			// encoding which has a length prefix in front of the
			// CommitSig.
			commitDiff, err := legacy.DeserializeCommitDiff(
				bytes.NewReader(commitDiffBytes),
			)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = current.SerializeCommitDiff(&b, commitDiff)
			if err != nil {
				return err
			}

			err = chanBucket.Put(commitDiffKey, b.Bytes())
			if err != nil {
				return err
			}
		}

		// With the commit diff migrated, we'll now check to see if
		// there're any un-acked updates we need to migrate as well.
		updateBytes := chanBucket.Get(unsignedAckedUpdatesKey)
		if updateBytes != nil {
			// We have un-acked updates we need to migrate so we'll
			// decode then re-encode them here using the new
			// format.
			legacyUnackedUpdates, err := legacy.DeserializeLogUpdates(
				bytes.NewReader(updateBytes),
			)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = current.SerializeLogUpdates(&b, legacyUnackedUpdates)
			if err != nil {
				return err
			}

			err = chanBucket.Put(unsignedAckedUpdatesKey, b.Bytes())
			if err != nil {
				return err
			}
		}

		// Remote unsiged updates as well.
		updateBytes = chanBucket.Get(remoteUnsignedLocalUpdatesKey)
		if updateBytes != nil {
			legacyUnsignedUpdates, err := legacy.DeserializeLogUpdates(
				bytes.NewReader(updateBytes),
			)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = current.SerializeLogUpdates(&b, legacyUnsignedUpdates)
			if err != nil {
				return err
			}

			err = chanBucket.Put(remoteUnsignedLocalUpdatesKey, b.Bytes())
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func migrateCloseChanSummaries(tx kvdb.RwTx) error {
	closedChanBucket := tx.ReadWriteBucket(closedChannelBucket)

	// Exit early if bucket is not found.
	if closedChannelBucket == nil {
		return nil
	}

	type closedChan struct {
		chanKey      []byte
		summaryBytes []byte
	}
	var closedChans []closedChan
	err := closedChanBucket.ForEach(func(k, v []byte) error {
		closedChans = append(closedChans, closedChan{
			chanKey:      k,
			summaryBytes: v,
		})
		return nil
	})
	if err != nil {
		return err
	}

	for _, closedChan := range closedChans {
		oldSummary, err := legacy.DeserializeCloseChannelSummary(
			bytes.NewReader(closedChan.summaryBytes),
		)
		if err != nil {
			return err
		}

		var newSummaryBytes bytes.Buffer
		err = current.SerializeChannelCloseSummary(
			&newSummaryBytes, oldSummary,
		)
		if err != nil {
			return err
		}

		err = closedChanBucket.Put(
			closedChan.chanKey, newSummaryBytes.Bytes(),
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func migrateForwardingPackages(tx kvdb.RwTx) error {
	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)

	// Exit early if bucket is not found.
	if fwdPkgBkt == nil {
		return nil
	}

	// We Go through the bucket and fetches all short channel IDs.
	var sources []lnwire.ShortChannelID
	err := fwdPkgBkt.ForEach(func(k, v []byte) error {
		source := lnwire.NewShortChanIDFromInt(byteOrder.Uint64(k))
		sources = append(sources, source)
		return nil
	})
	if err != nil {
		return err
	}

	// Now load all forwading packages using the legacy encoding.
	var pkgsToMigrate []*common.FwdPkg
	for _, source := range sources {
		packager := legacy.NewChannelPackager(source)
		fwdPkgs, err := packager.LoadFwdPkgs(tx)
		if err != nil {
			return err
		}

		pkgsToMigrate = append(pkgsToMigrate, fwdPkgs...)
	}

	// Add back the packages using the current encoding.
	for _, pkg := range pkgsToMigrate {
		packager := current.NewChannelPackager(pkg.Source)
		err := packager.AddFwdPkg(tx, pkg)
		if err != nil {
			return err
		}
	}

	return nil
}

func migrateNetworkResults(tx kvdb.RwTx) error {
	networkResults := tx.ReadWriteBucket(networkResultStoreBucketKey)

	// Exit early if bucket is not found.
	if networkResults == nil {
		return nil
	}

	// Similar to the prior migrations, we'll do this one in two phases:
	// we'll first grab all the keys we need to migrate in one loop, then
	// update them all in another loop.
	var netResultsToMigrate [][2][]byte
	err := networkResults.ForEach(func(k, v []byte) error {
		netResultsToMigrate = append(netResultsToMigrate, [2][]byte{
			k, v,
		})
		return nil
	})
	if err != nil {
		return err
	}

	for _, netResult := range netResultsToMigrate {
		resKey := netResult[0]
		resBytes := netResult[1]
		oldResult, err := legacy.DeserializeNetworkResult(
			bytes.NewReader(resBytes),
		)
		if err != nil {
			return err
		}

		var newResultBuf bytes.Buffer
		err = current.SerializeNetworkResult(&newResultBuf, oldResult)
		if err != nil {
			return err
		}

		err = networkResults.Put(resKey, newResultBuf.Bytes())
		if err != nil {
			return err
		}
	}
	return nil
}
