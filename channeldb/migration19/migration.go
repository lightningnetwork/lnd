package migration19

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/kvdb"
)

// MigrateDatabaseWireMessages performs a migration in all areas that we
// currently store wire messages without length prefixes. This includes the
// CommitDiff struct, ChannelCloseSummary, LogUpdates, and also the
// networkResult struct as well.
func MigrateDatabaseWireMessages(tx kvdb.RwTx) error {
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil
	}

	// The migration will proceed in three phases: we'll need to update any
	// pending commit diffs, then any unsigned acked updates for all open
	// channels, then finally we'll need to update all the current
	// stored network results for payments in the switch.
	//
	// In this phase, we'll migrate the open channel data.
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
			commitDiff, err := deserializeCommitDiffLegacy(
				bytes.NewReader(commitDiffBytes),
			)
			if err != nil {
				return err
			}

			var b bytes.Buffer
			err = serializeCommitDiff(&b, commitDiff)
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
		if updateBytes == nil {
			return nil
		}

		// We have un-acked updates we need to migrate so we'll decode
		// then re-encode them here using the new format.
		legacyUnackedUpdates, err := deserializeLogUpdatesLegacy(
			bytes.NewReader(updateBytes),
		)
		if err != nil {
			return err
		}

		var ub bytes.Buffer
		err = serializeLogUpdates(&ub, legacyUnackedUpdates)
		if err != nil {
			return err
		}

		err = chanBucket.Put(unsignedAckedUpdatesKey, ub.Bytes())
		if err != nil {
			return err
		}
	}

	// Next, we'll update all the present close channel summaries as well.
	type closedChan struct {
		chanKey      []byte
		summaryBytes []byte
	}
	var closedChans []closedChan

	closedChanBucket := tx.ReadWriteBucket(closedChannelBucket)
	if closedChannelBucket == nil {
		return fmt.Errorf("no closed channels found")
	}
	err = closedChanBucket.ForEach(func(k, v []byte) error {
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
		oldSummary, err := deserializeCloseChannelSummaryLegacy(
			bytes.NewReader(closedChan.summaryBytes),
		)
		if err != nil {
			return err
		}

		var newSummaryBytes bytes.Buffer
		err = serializeChannelCloseSummary(
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

	// Finally, we'll update the pending network results as well.
	networkResults := tx.ReadWriteBucket(networkResultStoreBucketKey)
	if networkResults == nil {
		return nil
	}

	// Similar to the prior migrations, we'll do this one in two phases:
	// we'll first grab all the keys we need to migrate in one loop, then
	// update them all in another loop.
	var netResultsToMigrate [][2][]byte
	err = networkResults.ForEach(func(k, v []byte) error {
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
		oldResult, err := deserializeNetworkResultLegacy(
			bytes.NewReader(resBytes),
		)
		if err != nil {
			return err
		}

		var newResultBuf bytes.Buffer
		err = serializeNetworkResult(&newResultBuf, oldResult)
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
