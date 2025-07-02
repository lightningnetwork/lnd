package graphdb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/pmezard/go-difflib/difflib"
)

// ErrMigrationMismatch is returned when a migrated graph record does not match
// the original record.
var ErrMigrationMismatch = fmt.Errorf("migrated graph record does not match " +
	"original record")

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
		return false, fmt.Errorf("failed to check graph existence: %w",
			err)
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
		sort.Slice(node.Addresses, func(i, j int) bool {
			return node.Addresses[i].String() <
				node.Addresses[j].String()
		})
		sort.Slice(migratedNode.Addresses, func(i, j int) bool {
			return migratedNode.Addresses[i].String() <
				migratedNode.Addresses[j].String()
		})

		return compare(node, migratedNode, fmt.Sprintf("node %x", pub))
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
	err = compare(pub[:], srcNodes[0].PubKey, "source node")
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

		// Sanity check to ensure that the policy has valid extra opaque
		// data. If it does not, we'll skip it. We need to do this
		// because previously we would just persist any TLV bytes that
		// we received without validating them. Now, however, we
		// normalise the storage of extra opaque data, so we need to
		// ensure that the data is valid. We don't want to abort the
		// migration if we encounter a policy with invalid extra opaque
		// data, so we'll just skip it and log a warning.
		_, err := marshalExtraOpaqueData(policy.ExtraOpaqueData)
		if errors.Is(err, ErrParsingExtraTLVBytes) {
			skippedPolicyCount++
			log.Warnf("Skipping policy for channel %d with "+
				"invalid extra opaque data: %v",
				policy.ChannelID, policy.ExtraOpaqueData)

			return nil
		} else if err != nil {
			return fmt.Errorf("unable to marshal extra opaque "+
				"data: %w. %+v", err, policy.ExtraOpaqueData)
		}

		policyCount++

		_, _, _, err = updateChanEdgePolicy(ctx, sqlDB, policy)
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
				"data for channel %d: %w %v", scid, err,
				channel.ExtraOpaqueData)
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
	err = compare(dbChanInfo.channelID, row.Channel.ID, "channel DB ID")
	if err != nil {
		return err
	}
	err = compare(dbChanInfo.node1ID, row.Node.ID, "node1 DB ID")
	if err != nil {
		return err
	}
	err = compare(dbChanInfo.node2ID, row.Node_2.ID, "node2 DB ID")
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

	err = compare(channel, migChan, fmt.Sprintf("channel %d", scid))
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

		return compare(*expPolicy, *migPolicy, "channel policy")
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

		return compare(hash[:], migratedHash, "prune log entry")
	}

	// Iterate over each prune log entry in the KV store and migrate it to
	// the SQL database.
	err := forEachPruneLogEntry(kvBackend,
		func(height uint32, hash *chainhash.Hash) error {
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
		row.Node.PubKey, row.Node_2.PubKey,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	edge, err := getAndBuildEdgeInfo(
		ctx, db, chain, row.Channel.ID, row.Channel, node1, node2,
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

// compare checks if the original and migrated objects are equal. If they
// are not, it returns an error with a unified diff of the two objects.
func compare(original, migrated any, identifier string) error {
	if reflect.DeepEqual(original, migrated) {
		return nil
	}

	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(spew.Sdump(original)),
		B:        difflib.SplitLines(spew.Sdump(migrated)),
		FromFile: "Expected",
		FromDate: "",
		ToFile:   "Actual",
		ToDate:   "",
		Context:  3,
	}
	diffText, _ := difflib.GetUnifiedDiffString(diff)

	return fmt.Errorf("%w: %s.\n%v", ErrMigrationMismatch, identifier,
		diffText)
}
