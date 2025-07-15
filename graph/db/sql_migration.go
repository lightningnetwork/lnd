package graphdb

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// MigrateGraphToSQL migrates the graph store from a KV backend to a SQL
// backend.
//
// NOTE: this is currently not called from any code path. It is called via tests
// only for now and will be called from the main lnd binary once the
// migration is fully implemented and tested.
func MigrateGraphToSQL(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries, chain chainhash.Hash) error {

	log.Infof("Starting migration of the graph store from KV to SQL")
	t0 := time.Now()

	// Check if there is a graph to migrate.
	graphExists, err := checkGraphExists(kvBackend)
	if err != nil {
		return fmt.Errorf("failed to check graph existence: %w", err)
	}
	if !graphExists {
		log.Infof("No graph found in KV store, skipping the migration")
		return nil
	}

	// 1) Migrate all the nodes.
	if err := migrateNodes(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	// 2) Migrate the source node.
	if err := migrateSourceNode(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate source node: %w", err)
	}

	// 3) Migrate all the channels and channel policies.
	err = migrateChannelsAndPolicies(ctx, kvBackend, sqlDB, chain)
	if err != nil {
		return fmt.Errorf("could not migrate channels and policies: %w",
			err)
	}

	// 4) Migrate the Prune log.
	if err := migratePruneLog(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate prune log: %w", err)
	}

	// 5) Migrate the closed SCID index.
	err = migrateClosedSCIDIndex(ctx, kvBackend, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate closed SCID index: %w",
			err)
	}

	// 6) Migrate the zombie index.
	if err := migrateZombieIndex(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate zombie index: %w", err)
	}

	log.Infof("Finished migration of the graph store from KV to SQL in %v",
		time.Since(t0))

	return nil
}

// checkGraphExists checks if the graph exists in the KV backend.
func checkGraphExists(db kvdb.Backend) (bool, error) {
	// Check if there is even a graph to migrate.
	err := db.View(func(tx kvdb.RTx) error {
		// Check for the existence of the node bucket which is a top
		// level bucket that would have been created on the initial
		// creation of the graph store.
		nodes := tx.ReadBucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}

		return nil
	}, func() {})
	if errors.Is(err, ErrGraphNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// migrateNodes migrates all nodes from the KV backend to the SQL database.
// This includes doing a sanity check after each migration to ensure that the
// migrated node matches the original node.
func migrateNodes(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries) error {

	// Keep track of the number of nodes migrated and the number of
	// nodes skipped due to errors.
	var (
		count   uint64
		skipped uint64
	)

	// Loop through each node in the KV store and insert it into the SQL
	// database.
	err := forEachNode(kvBackend, func(_ kvdb.RTx,
		node *models.LightningNode) error {

		pub := node.PubKeyBytes

		// Sanity check to ensure that the node has valid extra opaque
		// data. If it does not, we'll skip it. We need to do this
		// because previously we would just persist any TLV bytes that
		// we received without validating them. Now, however, we
		// normalise the storage of extra opaque data, so we need to
		// ensure that the data is valid. We don't want to abort the
		// migration if we encounter a node with invalid extra opaque
		// data, so we'll just skip it and log a warning.
		_, err := marshalExtraOpaqueData(node.ExtraOpaqueData)
		if errors.Is(err, ErrParsingExtraTLVBytes) {
			skipped++
			log.Warnf("Skipping migration of node %x with invalid "+
				"extra opaque data: %v", pub,
				node.ExtraOpaqueData)

			return nil
		} else if err != nil {
			return fmt.Errorf("unable to marshal extra "+
				"opaque data for node %x: %w", pub, err)
		}

		count++

		// TODO(elle): At this point, we should check the loaded node
		// to see if we should extract any DNS addresses from its
		// opaque type addresses. This is expected to be done in:
		// https://github.com/lightningnetwork/lnd/pull/9455.
		// This TODO is being tracked in
		//  https://github.com/lightningnetwork/lnd/issues/9795 as this
		// must be addressed before making this code path active in
		// production.

		// Write the node to the SQL database.
		id, err := upsertNode(ctx, sqlDB, node)
		if err != nil {
			return fmt.Errorf("could not persist node(%x): %w", pub,
				err)
		}

		// Fetch it from the SQL store and compare it against the
		// original node object to ensure the migration was successful.
		dbNode, err := sqlDB.GetNodeByPubKey(
			ctx, sqlc.GetNodeByPubKeyParams{
				PubKey:  node.PubKeyBytes[:],
				Version: int16(ProtocolV1),
			},
		)
		if err != nil {
			return fmt.Errorf("could not get node by pubkey (%x)"+
				"after migration: %w", pub, err)
		}

		// Sanity check: ensure the migrated node ID matches the one we
		// just inserted.
		if dbNode.ID != id {
			return fmt.Errorf("node ID mismatch for node (%x) "+
				"after migration: expected %d, got %d",
				pub, id, dbNode.ID)
		}

		migratedNode, err := buildNode(ctx, sqlDB, &dbNode)
		if err != nil {
			return fmt.Errorf("could not build migrated node "+
				"from dbNode(db id: %d, node pub: %x): %w",
				dbNode.ID, pub, err)
		}

		// Make sure that the node addresses are sorted before
		// comparing them to ensure that the order of addresses does
		// not affect the comparison.
		slices.SortFunc(node.Addresses, func(i, j net.Addr) int {
			return cmp.Compare(i.String(), j.String())
		})
		slices.SortFunc(
			migratedNode.Addresses, func(i, j net.Addr) int {
				return cmp.Compare(i.String(), j.String())
			},
		)

		return sqldb.CompareRecords(
			node, migratedNode, fmt.Sprintf("node %x", pub),
		)
	}, func() {
		// No reset is needed since if a retry occurs, the entire
		// migration will be retried from the start.
	})
	if err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	log.Infof("Migrated %d nodes from KV to SQL (skipped %d nodes due to "+
		"invalid TLV streams)", count, skipped)

	return nil
}

// migrateSourceNode migrates the source node from the KV backend to the
// SQL database.
func migrateSourceNode(ctx context.Context, kvdb kvdb.Backend,
	sqlDB SQLQueries) error {

	sourceNode, err := sourceNode(kvdb)
	if errors.Is(err, ErrSourceNodeNotSet) {
		// If the source node has not been set yet, we can skip this
		// migration step.
		return nil
	} else if err != nil {
		return fmt.Errorf("could not get source node from kv "+
			"store: %w", err)
	}

	pub := sourceNode.PubKeyBytes

	// Get the DB ID of the source node by its public key. This node must
	// already exist in the SQL database, as it should have been migrated
	// in the previous node-migration step.
	id, err := sqlDB.GetNodeIDByPubKey(
		ctx, sqlc.GetNodeIDByPubKeyParams{
			PubKey:  pub[:],
			Version: int16(ProtocolV1),
		},
	)
	if err != nil {
		return fmt.Errorf("could not get source node ID: %w", err)
	}

	// Now we can add the source node to the SQL database.
	err = sqlDB.AddSourceNode(ctx, id)
	if err != nil {
		return fmt.Errorf("could not add source node to SQL store: %w",
			err)
	}

	// Verify that the source node was added correctly by fetching it back
	// from the SQL database and checking that the expected DB ID and
	// pub key are returned. We don't need to do a whole node comparison
	// here, as this was already done in the previous migration step.
	srcNodes, err := sqlDB.GetSourceNodesByVersion(ctx, int16(ProtocolV1))
	if err != nil {
		return fmt.Errorf("could not get source nodes from SQL "+
			"store: %w", err)
	}

	// The SQL store has support for multiple source nodes (for future
	// protocol versions) but this migration is purely aimed at the V1
	// store, and so we expect exactly one source node to be present.
	if len(srcNodes) != 1 {
		return fmt.Errorf("expected exactly one source node, "+
			"got %d", len(srcNodes))
	}

	// Check that the source node ID and pub key match the original
	// source node.
	if srcNodes[0].NodeID != id {
		return fmt.Errorf("source node ID mismatch after migration: "+
			"expected %d, got %d", id, srcNodes[0].NodeID)
	}
	err = sqldb.CompareRecords(pub[:], srcNodes[0].PubKey, "source node")
	if err != nil {
		return fmt.Errorf("source node pubkey mismatch after "+
			"migration: %w", err)
	}

	log.Infof("Migrated source node with pubkey %x to SQL", pub[:])

	return nil
}

// migrateChannelsAndPolicies migrates all channels and their policies
// from the KV backend to the SQL database.
func migrateChannelsAndPolicies(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries, chain chainhash.Hash) error {

	var (
		channelCount       uint64
		skippedChanCount   uint64
		policyCount        uint64
		skippedPolicyCount uint64
	)
	migChanPolicy := func(policy *models.ChannelEdgePolicy) error {
		// If the policy is nil, we can skip it.
		if policy == nil {
			return nil
		}

		// Unlike the special case of invalid TLV bytes for node and
		// channel announcements, we don't need to handle the case for
		// channel policies here because it is already handled in the
		// `forEachChannel` function. If the policy has invalid TLV
		// bytes, then `nil` will be passed to this function.

		policyCount++

		_, _, _, err := updateChanEdgePolicy(ctx, sqlDB, policy)
		if err != nil {
			return fmt.Errorf("could not migrate channel "+
				"policy %d: %w", policy.ChannelID, err)
		}

		return nil
	}

	// Iterate over each channel in the KV store and migrate it and its
	// policies to the SQL database.
	err := forEachChannel(kvBackend, func(channel *models.ChannelEdgeInfo,
		policy1 *models.ChannelEdgePolicy,
		policy2 *models.ChannelEdgePolicy) error {

		scid := channel.ChannelID

		// Here, we do a sanity check to ensure that the chain hash of
		// the channel returned by the KV store matches the expected
		// chain hash. This is important since in the SQL store, we will
		// no longer explicitly store the chain hash in the channel
		// info, but rather rely on the chain hash LND is running with.
		// So this is our way of ensuring that LND is running on the
		// correct network at migration time.
		if channel.ChainHash != chain {
			return fmt.Errorf("channel %d has chain hash %s, "+
				"expected %s", scid, channel.ChainHash, chain)
		}

		// Sanity check to ensure that the channel has valid extra
		// opaque data. If it does not, we'll skip it. We need to do
		// this because previously we would just persist any TLV bytes
		// that we received without validating them. Now, however, we
		// normalise the storage of extra opaque data, so we need to
		// ensure that the data is valid. We don't want to abort the
		// migration if we encounter a channel with invalid extra opaque
		// data, so we'll just skip it and log a warning.
		_, err := marshalExtraOpaqueData(channel.ExtraOpaqueData)
		if errors.Is(err, ErrParsingExtraTLVBytes) {
			log.Warnf("Skipping channel %d with invalid "+
				"extra opaque data: %v", scid,
				channel.ExtraOpaqueData)

			skippedChanCount++

			// If we skip a channel, we also skip its policies.
			if policy1 != nil {
				skippedPolicyCount++
			}
			if policy2 != nil {
				skippedPolicyCount++
			}

			return nil
		} else if err != nil {
			return fmt.Errorf("unable to marshal extra opaque "+
				"data for channel %d (%v): %w", scid,
				channel.ExtraOpaqueData, err)
		}

		channelCount++
		err = migrateSingleChannel(
			ctx, sqlDB, channel, policy1, policy2, migChanPolicy,
		)
		if err != nil {
			return fmt.Errorf("could not migrate channel %d: %w",
				scid, err)
		}

		return nil
	}, func() {
		// No reset is needed since if a retry occurs, the entire
		// migration will be retried from the start.
	})
	if err != nil {
		return fmt.Errorf("could not migrate channels and policies: %w",
			err)
	}

	log.Infof("Migrated %d channels and %d policies from KV to SQL "+
		"(skipped %d channels and %d policies due to invalid TLV "+
		"streams)", channelCount, policyCount, skippedChanCount,
		skippedPolicyCount)

	return nil
}

func migrateSingleChannel(ctx context.Context, sqlDB SQLQueries,
	channel *models.ChannelEdgeInfo,
	policy1, policy2 *models.ChannelEdgePolicy,
	migChanPolicy func(*models.ChannelEdgePolicy) error) error {

	scid := channel.ChannelID

	// First, migrate the channel info along with its policies.
	dbChanInfo, err := insertChannel(ctx, sqlDB, channel)
	if err != nil {
		return fmt.Errorf("could not insert record for channel %d "+
			"in SQL store: %w", scid, err)
	}

	// Now, migrate the two channel policies.
	err = migChanPolicy(policy1)
	if err != nil {
		return fmt.Errorf("could not migrate policy1(%d): %w", scid,
			err)
	}
	err = migChanPolicy(policy2)
	if err != nil {
		return fmt.Errorf("could not migrate policy2(%d): %w", scid,
			err)
	}

	// Now, fetch the channel and its policies from the SQL DB.
	row, err := sqlDB.GetChannelBySCIDWithPolicies(
		ctx, sqlc.GetChannelBySCIDWithPoliciesParams{
			Scid:    channelIDToBytes(scid),
			Version: int16(ProtocolV1),
		},
	)
	if err != nil {
		return fmt.Errorf("could not get channel by SCID(%d): %w", scid,
			err)
	}

	// Assert that the DB IDs for the channel and nodes are as expected
	// given the inserted channel info.
	err = sqldb.CompareRecords(
		dbChanInfo.channelID, row.GraphChannel.ID, "channel DB ID",
	)
	if err != nil {
		return err
	}
	err = sqldb.CompareRecords(
		dbChanInfo.node1ID, row.GraphNode.ID, "node1 DB ID",
	)
	if err != nil {
		return err
	}
	err = sqldb.CompareRecords(
		dbChanInfo.node2ID, row.GraphNode_2.ID, "node2 DB ID",
	)
	if err != nil {
		return err
	}

	migChan, migPol1, migPol2, err := getAndBuildChanAndPolicies(
		ctx, sqlDB, row, channel.ChainHash,
	)
	if err != nil {
		return fmt.Errorf("could not build migrated channel and "+
			"policies: %w", err)
	}

	// Finally, compare the original channel info and
	// policies with the migrated ones to ensure they match.
	if len(channel.ExtraOpaqueData) == 0 {
		channel.ExtraOpaqueData = nil
	}
	if len(migChan.ExtraOpaqueData) == 0 {
		migChan.ExtraOpaqueData = nil
	}

	err = sqldb.CompareRecords(
		channel, migChan, fmt.Sprintf("channel %d", scid),
	)
	if err != nil {
		return err
	}

	checkPolicy := func(expPolicy,
		migPolicy *models.ChannelEdgePolicy) error {

		switch {
		// Both policies are nil, nothing to compare.
		case expPolicy == nil && migPolicy == nil:
			return nil

		// One of the policies is nil, but the other is not.
		case expPolicy == nil || migPolicy == nil:
			return fmt.Errorf("expected both policies to be "+
				"non-nil. Got expPolicy: %v, "+
				"migPolicy: %v", expPolicy, migPolicy)

		// Both policies are non-nil, we can compare them.
		default:
		}

		if len(expPolicy.ExtraOpaqueData) == 0 {
			expPolicy.ExtraOpaqueData = nil
		}
		if len(migPolicy.ExtraOpaqueData) == 0 {
			migPolicy.ExtraOpaqueData = nil
		}

		return sqldb.CompareRecords(
			*expPolicy, *migPolicy, "channel policy",
		)
	}

	err = checkPolicy(policy1, migPol1)
	if err != nil {
		return fmt.Errorf("policy1 mismatch for channel %d: %w", scid,
			err)
	}

	err = checkPolicy(policy2, migPol2)
	if err != nil {
		return fmt.Errorf("policy2 mismatch for channel %d: %w", scid,
			err)
	}

	return nil
}

// migratePruneLog migrates the prune log from the KV backend to the SQL
// database. It iterates over each prune log entry in the KV store, inserts it
// into the SQL database, and then verifies that the entry was inserted
// correctly by fetching it back from the SQL database and comparing it to the
// original entry.
func migratePruneLog(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries) error {

	var (
		count          uint64
		pruneTipHeight uint32
		pruneTipHash   chainhash.Hash
	)

	// migrateSinglePruneEntry is a helper function that inserts a single
	// prune log entry into the SQL database and verifies that it was
	// inserted correctly.
	migrateSinglePruneEntry := func(height uint32,
		hash *chainhash.Hash) error {

		count++

		// Keep track of the prune tip height and hash.
		if height > pruneTipHeight {
			pruneTipHeight = height
			pruneTipHash = *hash
		}

		err := sqlDB.UpsertPruneLogEntry(
			ctx, sqlc.UpsertPruneLogEntryParams{
				BlockHeight: int64(height),
				BlockHash:   hash[:],
			},
		)
		if err != nil {
			return fmt.Errorf("unable to insert prune log "+
				"entry for height %d: %w", height, err)
		}

		// Now, check that the entry was inserted correctly.
		migratedHash, err := sqlDB.GetPruneHashByHeight(
			ctx, int64(height),
		)
		if err != nil {
			return fmt.Errorf("could not get prune hash "+
				"for height %d: %w", height, err)
		}

		return sqldb.CompareRecords(
			hash[:], migratedHash, "prune log entry",
		)
	}

	// Iterate over each prune log entry in the KV store and migrate it to
	// the SQL database.
	err := forEachPruneLogEntry(
		kvBackend, func(height uint32, hash *chainhash.Hash) error {
			err := migrateSinglePruneEntry(height, hash)
			if err != nil {
				return fmt.Errorf("could not migrate "+
					"prune log entry at height %d: %w",
					height, err)
			}

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("could not migrate prune log: %w", err)
	}

	// Check that the prune tip is set correctly in the SQL
	// database.
	pruneTip, err := sqlDB.GetPruneTip(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		// The ErrGraphNeverPruned error is expected if no prune log
		// entries were migrated from the kvdb store. Otherwise, it's
		// an unexpected error.
		if count == 0 {
			log.Infof("No prune log entries found in KV store " +
				"to migrate")
			return nil
		}
		// Fall-through to the next error check.
	}
	if err != nil {
		return fmt.Errorf("could not get prune tip: %w", err)
	}

	if pruneTip.BlockHeight != int64(pruneTipHeight) ||
		!bytes.Equal(pruneTip.BlockHash, pruneTipHash[:]) {

		return fmt.Errorf("prune tip mismatch after migration: "+
			"expected height %d, hash %s; got height %d, "+
			"hash %s", pruneTipHeight, pruneTipHash,
			pruneTip.BlockHeight,
			chainhash.Hash(pruneTip.BlockHash))
	}

	log.Infof("Migrated %d prune log entries from KV to SQL. The prune "+
		"tip is: height %d, hash: %s", count, pruneTipHeight,
		pruneTipHash)

	return nil
}

// getAndBuildChanAndPolicies is a helper that builds the channel edge info
// and policies from the given row returned by the SQL query
// GetChannelBySCIDWithPolicies.
func getAndBuildChanAndPolicies(ctx context.Context, db SQLQueries,
	row sqlc.GetChannelBySCIDWithPoliciesRow,
	chain chainhash.Hash) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	node1, node2, err := buildNodeVertices(
		row.GraphNode.PubKey, row.GraphNode_2.PubKey,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	edge, err := getAndBuildEdgeInfo(
		ctx, db, chain, row.GraphChannel.ID, row.GraphChannel, node1,
		node2,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to build channel "+
			"info: %w", err)
	}

	dbPol1, dbPol2, err := extractChannelPolicies(row)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to extract channel "+
			"policies: %w", err)
	}

	policy1, policy2, err := getAndBuildChanPolicies(
		ctx, db, dbPol1, dbPol2, edge.ChannelID, node1, node2,
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to build channel "+
			"policies: %w", err)
	}

	return edge, policy1, policy2, nil
}

// forEachPruneLogEntry iterates over each prune log entry in the KV
// backend and calls the provided callback function for each entry.
func forEachPruneLogEntry(db kvdb.Backend, cb func(height uint32,
	hash *chainhash.Hash) error) error {

	return kvdb.View(db, func(tx kvdb.RTx) error {
		metaBucket := tx.ReadBucket(graphMetaBucket)
		if metaBucket == nil {
			return ErrGraphNotFound
		}

		pruneBucket := metaBucket.NestedReadBucket(pruneLogBucket)
		if pruneBucket == nil {
			// The graph has never been pruned and so, there are no
			// entries to iterate over.
			return nil
		}

		return pruneBucket.ForEach(func(k, v []byte) error {
			blockHeight := byteOrder.Uint32(k)
			var blockHash chainhash.Hash
			copy(blockHash[:], v)

			return cb(blockHeight, &blockHash)
		})
	}, func() {})
}

// migrateClosedSCIDIndex migrates the closed SCID index from the KV backend to
// the SQL database. It iterates over each closed SCID in the KV store, inserts
// it into the SQL database, and then verifies that the SCID was inserted
// correctly by checking if the channel with the given SCID is seen as closed in
// the SQL database.
func migrateClosedSCIDIndex(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries) error {

	var count uint64
	migrateSingleClosedSCID := func(scid lnwire.ShortChannelID) error {
		count++

		chanIDB := channelIDToBytes(scid.ToUint64())
		err := sqlDB.InsertClosedChannel(ctx, chanIDB)
		if err != nil {
			return fmt.Errorf("could not insert closed channel "+
				"with SCID %s: %w", scid, err)
		}

		// Now, verify that the channel with the given SCID is
		// seen as closed.
		isClosed, err := sqlDB.IsClosedChannel(ctx, chanIDB)
		if err != nil {
			return fmt.Errorf("could not check if channel %s "+
				"is closed: %w", scid, err)
		}

		if !isClosed {
			return fmt.Errorf("channel %s should be closed, "+
				"but is not", scid)
		}

		return nil
	}

	err := forEachClosedSCID(kvBackend, migrateSingleClosedSCID)
	if err != nil {
		return fmt.Errorf("could not migrate closed SCID index: %w",
			err)
	}

	log.Infof("Migrated %d closed SCIDs from KV to SQL", count)

	return nil
}

// migrateZombieIndex migrates the zombie index from the KV backend to
// the SQL database. It iterates over each zombie channel in the KV store,
// inserts it into the SQL database, and then verifies that the channel is
// indeed marked as a zombie channel in the SQL database.
//
// NOTE: before inserting an entry into the zombie index, the function checks
// if the channel is already marked as closed in the SQL store. If it is,
// the entry is skipped. This means that the resulting zombie index count in
// the SQL store may well be less than the count of zombie channels in the KV
// store.
func migrateZombieIndex(ctx context.Context, kvBackend kvdb.Backend,
	sqlDB SQLQueries) error {

	var count uint64
	err := forEachZombieEntry(kvBackend, func(chanID uint64, pubKey1,
		pubKey2 [33]byte) error {

		chanIDB := channelIDToBytes(chanID)

		// If it is in the closed SCID index, we don't need to
		// add it to the zombie index.
		//
		// NOTE: this means that the resulting zombie index count in
		// the SQL store may well be less than the count of zombie
		// channels in the KV store.
		isClosed, err := sqlDB.IsClosedChannel(ctx, chanIDB)
		if err != nil {
			return fmt.Errorf("could not check closed "+
				"channel: %w", err)
		}
		if isClosed {
			return nil
		}

		count++

		err = sqlDB.UpsertZombieChannel(
			ctx, sqlc.UpsertZombieChannelParams{
				Version:  int16(ProtocolV1),
				Scid:     chanIDB,
				NodeKey1: pubKey1[:],
				NodeKey2: pubKey2[:],
			},
		)
		if err != nil {
			return fmt.Errorf("could not upsert zombie "+
				"channel %d: %w", chanID, err)
		}

		// Finally, verify that the channel is indeed marked as a
		// zombie channel.
		isZombie, err := sqlDB.IsZombieChannel(
			ctx, sqlc.IsZombieChannelParams{
				Version: int16(ProtocolV1),
				Scid:    chanIDB,
			},
		)
		if err != nil {
			return fmt.Errorf("could not check if "+
				"channel %d is zombie: %w", chanID, err)
		}

		if !isZombie {
			return fmt.Errorf("channel %d should be "+
				"a zombie, but is not", chanID)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not migrate zombie index: %w", err)
	}

	log.Infof("Migrated %d zombie channels from KV to SQL", count)

	return nil
}

// forEachZombieEntry iterates over each zombie channel entry in the
// KV backend and calls the provided callback function for each entry.
func forEachZombieEntry(db kvdb.Backend, cb func(chanID uint64, pubKey1,
	pubKey2 [33]byte) error) error {

	return kvdb.View(db, func(tx kvdb.RTx) error {
		edges := tx.ReadBucket(edgeBucket)
		if edges == nil {
			return ErrGraphNoEdgesFound
		}
		zombieIndex := edges.NestedReadBucket(zombieBucket)
		if zombieIndex == nil {
			return nil
		}

		return zombieIndex.ForEach(func(k, v []byte) error {
			var pubKey1, pubKey2 [33]byte
			copy(pubKey1[:], v[:33])
			copy(pubKey2[:], v[33:])

			return cb(byteOrder.Uint64(k), pubKey1, pubKey2)
		})
	}, func() {})
}

// forEachClosedSCID iterates over each closed SCID in the KV backend and calls
// the provided callback function for each SCID.
func forEachClosedSCID(db kvdb.Backend,
	cb func(lnwire.ShortChannelID) error) error {

	return kvdb.View(db, func(tx kvdb.RTx) error {
		closedScids := tx.ReadBucket(closedScidBucket)
		if closedScids == nil {
			return nil
		}

		return closedScids.ForEach(func(k, _ []byte) error {
			return cb(lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(k),
			))
		})
	}, func() {})
}
