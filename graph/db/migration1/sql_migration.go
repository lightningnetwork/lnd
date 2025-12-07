package migration1

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image/color"
	"net"
	"slices"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/graph/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"golang.org/x/time/rate"
)

// MigrateGraphToSQL migrates the graph store from a KV backend to a SQL
// backend.
//
// NOTE: this is currently not called from any code path. It is called via tests
// only for now and will be called from the main lnd binary once the
// migration is fully implemented and tested.
func MigrateGraphToSQL(ctx context.Context, cfg *SQLStoreConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

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
	err = migrateNodes(ctx, cfg.QueryCfg, kvBackend, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	// 2) Migrate the source node.
	if err := migrateSourceNode(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate source node: %w", err)
	}

	// 3) Migrate all the channels and channel policies.
	err = migrateChannelsAndPolicies(ctx, cfg, kvBackend, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate channels and policies: %w",
			err)
	}

	// 4) Migrate the Prune log.
	err = migratePruneLog(ctx, cfg.QueryCfg, kvBackend, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate prune log: %w", err)
	}

	// 5) Migrate the closed SCID index.
	err = migrateClosedSCIDIndex(ctx, cfg.QueryCfg, kvBackend, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate closed SCID index: %w",
			err)
	}

	// 6) Migrate the zombie index.
	err = migrateZombieIndex(ctx, cfg.QueryCfg, kvBackend, sqlDB)
	if err != nil {
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
// It collects nodes in batches, inserts them individually, and then validates
// them in batches.
func migrateNodes(ctx context.Context, cfg *sqldb.QueryConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

	// Keep track of the number of nodes migrated and the number of
	// nodes skipped due to errors.
	var (
		totalTime = time.Now()

		count   uint64
		skipped uint64

		t0    = time.Now()
		chunk uint64
		s     = rate.Sometimes{
			Interval: 10 * time.Second,
		}
	)

	// batch is a map that holds node objects that have been migrated to
	// the native SQL store that have yet to be validated. The object's held
	// by this map were derived from the KVDB store and so when they are
	// validated, the map index (the SQL store node ID) will be used to
	// fetch the corresponding node object in the SQL store, and it will
	// then be compared against the original KVDB node object.
	batch := make(
		map[int64]*models.Node, cfg.MaxBatchSize,
	)

	// validateBatch validates that the batch of nodes in the 'batch' map
	// have been migrated successfully.
	validateBatch := func() error {
		if len(batch) == 0 {
			return nil
		}

		// Extract DB node IDs.
		dbIDs := make([]int64, 0, len(batch))
		for dbID := range batch {
			dbIDs = append(dbIDs, dbID)
		}

		// Batch fetch all nodes from the database.
		dbNodes, err := sqlDB.GetNodesByIDs(ctx, dbIDs)
		if err != nil {
			return fmt.Errorf("could not batch fetch nodes: %w",
				err)
		}

		// Make sure that the number of nodes fetched matches the number
		// of nodes in the batch.
		if len(dbNodes) != len(batch) {
			return fmt.Errorf("expected to fetch %d nodes, "+
				"but got %d", len(batch), len(dbNodes))
		}

		// Now, batch fetch the normalised data for all the nodes in
		// the batch.
		batchData, err := batchLoadNodeData(ctx, cfg, sqlDB, dbIDs)
		if err != nil {
			return fmt.Errorf("unable to batch load node data: %w",
				err)
		}

		for _, dbNode := range dbNodes {
			// Get the KVDB node info from the batch map.
			node, ok := batch[dbNode.ID]
			if !ok {
				return fmt.Errorf("node with ID %d not found "+
					"in batch", dbNode.ID)
			}

			// Build the migrated node from the DB node and the
			// batch node data.
			migNode, err := buildNodeWithBatchData(
				dbNode, batchData,
			)
			if err != nil {
				return fmt.Errorf("could not build migrated "+
					"node from dbNode(db id: %d, node "+
					"pub: %x): %w", dbNode.ID,
					node.PubKeyBytes, err)
			}

			// Make sure that the node addresses are sorted before
			// comparing them to ensure that the order of addresses
			// does not affect the comparison.
			slices.SortFunc(
				node.Addresses, func(i, j net.Addr) int {
					return cmp.Compare(
						i.String(), j.String(),
					)
				},
			)
			slices.SortFunc(
				migNode.Addresses, func(i, j net.Addr) int {
					return cmp.Compare(
						i.String(), j.String(),
					)
				},
			)

			err = sqldb.CompareRecords(
				node, migNode,
				fmt.Sprintf("node %x", node.PubKeyBytes),
			)
			if err != nil {
				return fmt.Errorf("node mismatch after "+
					"migration for node %x: %w",
					node.PubKeyBytes, err)
			}
		}

		// Clear the batch map for the next iteration.
		batch = make(
			map[int64]*models.Node, cfg.MaxBatchSize,
		)

		return nil
	}

	// Loop through each node in the KV store and insert it into the SQL
	// database.
	err := forEachNode(kvBackend, func(_ kvdb.RTx,
		node *models.Node) error {

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

		if err = maybeOverrideNodeAddresses(node); err != nil {
			skipped++
			log.Warnf("Skipping migration of node %x with invalid "+
				"address (%v): %v", pub, node.Addresses, err)

			return nil
		}

		count++
		chunk++

		// Write the node to the SQL database.
		id, err := insertNodeSQLMig(ctx, sqlDB, node)
		if err != nil {
			return fmt.Errorf("could not persist node(%x): %w", pub,
				err)
		}

		// Add to validation batch.
		batch[id] = node

		// Validate batch when full.
		if len(batch) >= int(cfg.MaxBatchSize) {
			err := validateBatch()
			if err != nil {
				return fmt.Errorf("batch validation failed: %w",
					err)
			}
		}

		s.Do(func() {
			elapsed := time.Since(t0).Seconds()
			ratePerSec := float64(chunk) / elapsed
			log.Debugf("Migrated %d nodes (%.2f nodes/sec)",
				count, ratePerSec)

			t0 = time.Now()
			chunk = 0
		})

		return nil
	}, func() {
		count = 0
		chunk = 0
		skipped = 0
		t0 = time.Now()
		batch = make(map[int64]*models.Node, cfg.MaxBatchSize)
	})
	if err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	// Validate any remaining nodes in the batch.
	if len(batch) > 0 {
		err := validateBatch()
		if err != nil {
			return fmt.Errorf("final batch validation failed: %w",
				err)
		}
	}

	log.Infof("Migrated %d nodes from KV to SQL in %v (skipped %d nodes "+
		"due to invalid TLV streams or invalid addresses)", count,
		time.Since(totalTime),

		skipped)

	return nil
}

// maybeOverrideNodeAddresses checks if the node has any opaque addresses that
// can be parsed. If so, it replaces the node's addresses with the parsed
// addresses. If the address is unparseable, it returns an error.
func maybeOverrideNodeAddresses(node *models.Node) error {
	// In the majority of cases, the number of node addresses will remain
	// unchanged, so we pre-allocate a slice of the same length.
	addrs := make([]net.Addr, 0, len(node.Addresses))

	// Iterate over each address in search of any opaque addresses that we
	// can inspect.
	for _, addr := range node.Addresses {
		opaque, ok := addr.(*lnwire.OpaqueAddrs)
		if !ok {
			// Any non-opaque address is left unchanged.
			addrs = append(addrs, addr)
			continue
		}

		// For each opaque address, we'll now attempt to parse out any
		// known addresses. We'll do this in a loop, as it's possible
		// that there are several addresses encoded in a single opaque
		// address.
		payload := opaque.Payload
		for len(payload) > 0 {
			var (
				r            = bytes.NewReader(payload)
				numAddrBytes = uint16(len(payload))
			)
			byteRead, readAddr, err := lnwire.ReadAddress(
				r, numAddrBytes,
			)
			if err != nil {
				return err
			}

			// If we were able to read an address, we'll add it to
			// our list of addresses.
			if readAddr != nil {
				addrs = append(addrs, readAddr)
			}

			// If the address we read was an opaque address, it
			// means we've hit an unknown address type, and it has
			// consumed the rest of the payload. We can break out
			// of the loop.
			if _, ok := readAddr.(*lnwire.OpaqueAddrs); ok {
				break
			}

			// If we've read all the bytes, we can also break.
			if byteRead >= numAddrBytes {
				break
			}

			// Otherwise, we'll advance our payload slice and
			// continue.
			payload = payload[byteRead:]
		}
	}

	// Override the node addresses if we have any.
	if len(addrs) != 0 {
		node.Addresses = addrs
	}

	return nil
}

// migrateSourceNode migrates the source node from the KV backend to the
// SQL database.
func migrateSourceNode(ctx context.Context, kvdb kvdb.Backend,
	sqlDB SQLQueries) error {

	log.Debugf("Migrating source node from KV to SQL")

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
			Version: int16(lnwire.GossipVersion1),
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
	srcNodes, err := sqlDB.GetSourceNodesByVersion(
		ctx, int16(lnwire.GossipVersion1),
	)
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

// migChanInfo holds the information about a channel and its policies.
type migChanInfo struct {
	// edge is the channel object as read from the KVDB source.
	edge *models.ChannelEdgeInfo

	// policy1 is the first channel policy for the channel as read from
	// the KVDB source.
	policy1 *models.ChannelEdgePolicy

	// policy2 is the second channel policy for the channel as read
	// from the KVDB source.
	policy2 *models.ChannelEdgePolicy

	// dbInfo holds location info (in the form of DB IDs) of the channel
	// and its policies in the native-SQL destination.
	dbInfo *dbChanInfo
}

// migrateChannelsAndPolicies migrates all channels and their policies
// from the KV backend to the SQL database.
func migrateChannelsAndPolicies(ctx context.Context, cfg *SQLStoreConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

	var (
		totalTime = time.Now()

		channelCount       uint64
		skippedChanCount   uint64
		policyCount        uint64
		skippedPolicyCount uint64

		t0    = time.Now()
		chunk uint64
		s     = rate.Sometimes{
			Interval: 10 * time.Second,
		}
	)
	migChanPolicy := func(dbChanInfo *dbChanInfo,
		policy *models.ChannelEdgePolicy) error {

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

		err := insertChanEdgePolicyMig(ctx, sqlDB, dbChanInfo, policy)
		if err != nil {
			return fmt.Errorf("could not migrate channel "+
				"policy %d: %w", policy.ChannelID, err)
		}

		return nil
	}

	// batch is used to collect migrated channel info that we will
	// batch-validate. Each entry is indexed by the DB ID of the channel
	// in the SQL database.
	batch := make(map[int64]*migChanInfo, cfg.QueryCfg.MaxBatchSize)

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
		if channel.ChainHash != cfg.ChainHash {
			return fmt.Errorf("channel %d has chain hash %s, "+
				"expected %s", scid, channel.ChainHash,
				cfg.ChainHash)
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
		chunk++

		// Migrate the channel info along with its policies.
		dbChanInfo, err := insertChannelMig(ctx, sqlDB, channel)
		if err != nil {
			return fmt.Errorf("could not insert record for "+
				"channel %d in SQL store: %w", scid, err)
		}

		// Now, migrate the two channel policies for the channel.
		err = migChanPolicy(dbChanInfo, policy1)
		if err != nil {
			return fmt.Errorf("could not migrate policy1(%d): %w",
				scid, err)
		}
		err = migChanPolicy(dbChanInfo, policy2)
		if err != nil {
			return fmt.Errorf("could not migrate policy2(%d): %w",
				scid, err)
		}

		// Collect the migrated channel info and policies in a batch for
		// later validation.
		batch[dbChanInfo.channelID] = &migChanInfo{
			edge:    channel,
			policy1: policy1,
			policy2: policy2,
			dbInfo:  dbChanInfo,
		}

		if len(batch) >= int(cfg.QueryCfg.MaxBatchSize) {
			// Do batch validation.
			err := validateMigratedChannels(ctx, cfg, sqlDB, batch)
			if err != nil {
				return fmt.Errorf("could not validate "+
					"channel batch: %w", err)
			}

			batch = make(
				map[int64]*migChanInfo,
				cfg.QueryCfg.MaxBatchSize,
			)
		}

		s.Do(func() {
			elapsed := time.Since(t0).Seconds()
			ratePerSec := float64(chunk) / elapsed
			log.Debugf("Migrated %d channels (%.2f channels/sec)",
				channelCount, ratePerSec)

			t0 = time.Now()
			chunk = 0
		})

		return nil
	}, func() {
		channelCount = 0
		policyCount = 0
		chunk = 0
		skippedChanCount = 0
		skippedPolicyCount = 0
		t0 = time.Now()
		batch = make(map[int64]*migChanInfo, cfg.QueryCfg.MaxBatchSize)
	})
	if err != nil {
		return fmt.Errorf("could not migrate channels and policies: %w",
			err)
	}

	if len(batch) > 0 {
		// Do a final batch validation for any remaining channels.
		err := validateMigratedChannels(ctx, cfg, sqlDB, batch)
		if err != nil {
			return fmt.Errorf("could not validate final channel "+
				"batch: %w", err)
		}

		batch = make(map[int64]*migChanInfo, cfg.QueryCfg.MaxBatchSize)
	}

	log.Infof("Migrated %d channels and %d policies from KV to SQL in %s"+
		"(skipped %d channels and %d policies due to invalid TLV "+
		"streams)", channelCount, policyCount, time.Since(totalTime),
		skippedChanCount, skippedPolicyCount)

	return nil
}

// validateMigratedChannels validates the channels in the batch after they have
// been migrated to the SQL database. It batch fetches all channels by their IDs
// and compares the migrated channels and their policies with the original ones
// to ensure they match using batch construction patterns.
func validateMigratedChannels(ctx context.Context, cfg *SQLStoreConfig,
	sqlDB SQLQueries, batch map[int64]*migChanInfo) error {

	// Convert batch keys (DB IDs) to an int slice for the batch query.
	dbChanIDs := make([]int64, 0, len(batch))
	for id := range batch {
		dbChanIDs = append(dbChanIDs, id)
	}

	// Batch fetch all channels with their policies.
	rows, err := sqlDB.GetChannelsByIDs(ctx, dbChanIDs)
	if err != nil {
		return fmt.Errorf("could not batch get channels by IDs: %w",
			err)
	}

	// Sanity check that the same number of channels were returned
	// as requested.
	if len(rows) != len(dbChanIDs) {
		return fmt.Errorf("expected to fetch %d channels, "+
			"but got %d", len(dbChanIDs), len(rows))
	}

	// Collect all policy IDs needed for batch data loading.
	dbPolicyIDs := make([]int64, 0, len(dbChanIDs)*2)

	for _, row := range rows {
		scid := byteOrder.Uint64(row.GraphChannel.Scid)

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return fmt.Errorf("could not extract channel policies"+
				" for SCID %d: %w", scid, err)
		}
		if dbPol1 != nil {
			dbPolicyIDs = append(dbPolicyIDs, dbPol1.ID)
		}
		if dbPol2 != nil {
			dbPolicyIDs = append(dbPolicyIDs, dbPol2.ID)
		}
	}

	// Batch load all channel and policy data (features, extras).
	batchData, err := batchLoadChannelData(
		ctx, cfg.QueryCfg, sqlDB, dbChanIDs, dbPolicyIDs,
	)
	if err != nil {
		return fmt.Errorf("could not batch load channel and policy "+
			"data: %w", err)
	}

	// Validate each channel in the batch using pre-loaded data.
	for _, row := range rows {
		kvdbChan, ok := batch[row.GraphChannel.ID]
		if !ok {
			return fmt.Errorf("channel with ID %d not found "+
				"in batch", row.GraphChannel.ID)
		}

		scid := byteOrder.Uint64(row.GraphChannel.Scid)

		err = validateMigratedChannelWithBatchData(
			cfg, scid, kvdbChan, row, batchData,
		)
		if err != nil {
			return fmt.Errorf("channel %d validation failed "+
				"after migration: %w", scid, err)
		}
	}

	return nil
}

// validateMigratedChannelWithBatchData validates a single migrated channel
// using pre-fetched batch data for optimal performance.
func validateMigratedChannelWithBatchData(cfg *SQLStoreConfig,
	scid uint64, info *migChanInfo, row sqlc.GetChannelsByIDsRow,
	batchData *batchChannelData) error {

	dbChanInfo := info.dbInfo
	channel := info.edge

	// Assert that the DB IDs for the channel and nodes are as expected
	// given the inserted channel info.
	err := sqldb.CompareRecords(
		dbChanInfo.channelID, row.GraphChannel.ID, "channel DB ID",
	)
	if err != nil {
		return err
	}
	err = sqldb.CompareRecords(
		dbChanInfo.node1ID, row.Node1ID, "node1 DB ID",
	)
	if err != nil {
		return err
	}
	err = sqldb.CompareRecords(
		dbChanInfo.node2ID, row.Node2ID, "node2 DB ID",
	)
	if err != nil {
		return err
	}

	// Build node vertices from the row data.
	node1, node2, err := buildNodeVertices(
		row.Node1PubKey, row.Node2PubKey,
	)
	if err != nil {
		return err
	}

	// Build channel info using batch data.
	migChan, err := buildEdgeInfoWithBatchData(
		cfg.ChainHash, row.GraphChannel, node1, node2, batchData,
	)
	if err != nil {
		return fmt.Errorf("could not build migrated channel info: %w",
			err)
	}

	// Extract channel policies from the row.
	dbPol1, dbPol2, err := extractChannelPolicies(row)
	if err != nil {
		return fmt.Errorf("could not extract channel policies: %w", err)
	}

	// Build channel policies using batch data.
	migPol1, migPol2, err := buildChanPoliciesWithBatchData(
		dbPol1, dbPol2, scid, node1, node2, batchData,
	)
	if err != nil {
		return fmt.Errorf("could not build migrated channel "+
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

	err = checkPolicy(info.policy1, migPol1)
	if err != nil {
		return fmt.Errorf("policy1 mismatch for channel %d: %w", scid,
			err)
	}

	err = checkPolicy(info.policy2, migPol2)
	if err != nil {
		return fmt.Errorf("policy2 mismatch for channel %d: %w", scid,
			err)
	}

	return nil
}

// migratePruneLog migrates the prune log from the KV backend to the SQL
// database. It collects entries in batches, inserts them individually, and then
// validates them in batches using GetPruneEntriesForHeights for better i
// performance.
func migratePruneLog(ctx context.Context, cfg *sqldb.QueryConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

	var (
		totalTime = time.Now()

		count          uint64
		pruneTipHeight uint32
		pruneTipHash   chainhash.Hash

		t0    = time.Now()
		chunk uint64
		s     = rate.Sometimes{
			Interval: 10 * time.Second,
		}
	)

	batch := make(map[uint32]chainhash.Hash, cfg.MaxBatchSize)

	// validateBatch validates a batch of prune entries using batch query.
	validateBatch := func() error {
		if len(batch) == 0 {
			return nil
		}

		// Extract heights for the batch query.
		heights := make([]int64, 0, len(batch))
		for height := range batch {
			heights = append(heights, int64(height))
		}

		// Batch fetch all entries from the database.
		rows, err := sqlDB.GetPruneEntriesForHeights(ctx, heights)
		if err != nil {
			return fmt.Errorf("could not batch get prune "+
				"entries: %w", err)
		}

		if len(rows) != len(batch) {
			return fmt.Errorf("expected to fetch %d prune "+
				"entries, but got %d", len(batch),
				len(rows))
		}

		// Validate each entry in the batch.
		for _, row := range rows {
			kvdbHash, ok := batch[uint32(row.BlockHeight)]
			if !ok {
				return fmt.Errorf("prune entry for height %d "+
					"not found in batch", row.BlockHeight)
			}

			err := sqldb.CompareRecords(
				kvdbHash[:], row.BlockHash,
				fmt.Sprintf("prune log entry at height %d",
					row.BlockHash),
			)
			if err != nil {
				return err
			}
		}

		// Reset the batch map for the next iteration.
		batch = make(map[uint32]chainhash.Hash, cfg.MaxBatchSize)

		return nil
	}

	// Iterate over each prune log entry in the KV store and migrate it to
	// the SQL database.
	err := forEachPruneLogEntry(
		kvBackend, func(height uint32, hash *chainhash.Hash) error {
			count++
			chunk++

			// Keep track of the prune tip height and hash.
			if height > pruneTipHeight {
				pruneTipHeight = height
				pruneTipHash = *hash
			}

			// Insert the entry (individual inserts for now).
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

			// Add to validation batch.
			batch[height] = *hash

			// Validate batch when full.
			if len(batch) >= int(cfg.MaxBatchSize) {
				err := validateBatch()
				if err != nil {
					return fmt.Errorf("batch "+
						"validation failed: %w", err)
				}
			}

			s.Do(func() {
				elapsed := time.Since(t0).Seconds()
				ratePerSec := float64(chunk) / elapsed
				log.Debugf("Migrated %d prune log "+
					"entries (%.2f entries/sec)",
					count, ratePerSec)

				t0 = time.Now()
				chunk = 0
			})

			return nil
		},
		func() {
			count = 0
			chunk = 0
			t0 = time.Now()
			batch = make(
				map[uint32]chainhash.Hash, cfg.MaxBatchSize,
			)
		},
	)
	if err != nil {
		return fmt.Errorf("could not migrate prune log: %w", err)
	}

	// Validate any remaining entries in the batch.
	if len(batch) > 0 {
		err := validateBatch()
		if err != nil {
			return fmt.Errorf("final batch validation failed: %w",
				err)
		}
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

	log.Infof("Migrated %d prune log entries from KV to SQL in %s. "+
		"The prune tip is: height %d, hash: %s", count,
		time.Since(totalTime), pruneTipHeight, pruneTipHash)

	return nil
}

// forEachPruneLogEntry iterates over each prune log entry in the KV
// backend and calls the provided callback function for each entry.
func forEachPruneLogEntry(db kvdb.Backend, cb func(height uint32,
	hash *chainhash.Hash) error, reset func()) error {

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
	}, reset)
}

// migrateClosedSCIDIndex migrates the closed SCID index from the KV backend to
// the SQL database. It collects SCIDs in batches, inserts them individually,
// and then validates them in batches using GetClosedChannelsSCIDs for better
// performance.
func migrateClosedSCIDIndex(ctx context.Context, cfg *sqldb.QueryConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

	var (
		totalTime = time.Now()

		count uint64

		t0    = time.Now()
		chunk uint64
		s     = rate.Sometimes{
			Interval: 10 * time.Second,
		}
	)

	batch := make([][]byte, 0, cfg.MaxBatchSize)

	// validateBatch validates a batch of closed SCIDs using batch query.
	validateBatch := func() error {
		if len(batch) == 0 {
			return nil
		}

		// Batch fetch all closed SCIDs from the database.
		dbSCIDs, err := sqlDB.GetClosedChannelsSCIDs(ctx, batch)
		if err != nil {
			return fmt.Errorf("could not batch get closed "+
				"SCIDs: %w", err)
		}

		// Create set of SCIDs that exist in the database for quick
		// lookup.
		dbSCIDSet := make(map[string]struct{})
		for _, scid := range dbSCIDs {
			dbSCIDSet[string(scid)] = struct{}{}
		}

		// Validate each SCID in the batch.
		for _, expectedSCID := range batch {
			if _, found := dbSCIDSet[string(expectedSCID)]; !found {
				return fmt.Errorf("closed SCID %x not found "+
					"in database", expectedSCID)
			}
		}

		// Reset the batch for the next iteration.
		batch = make([][]byte, 0, cfg.MaxBatchSize)

		return nil
	}

	migrateSingleClosedSCID := func(scid lnwire.ShortChannelID) error {
		count++
		chunk++

		chanIDB := channelIDToBytes(scid.ToUint64())
		err := sqlDB.InsertClosedChannel(ctx, chanIDB)
		if err != nil {
			return fmt.Errorf("could not insert closed channel "+
				"with SCID %s: %w", scid, err)
		}

		// Add to validation batch.
		batch = append(batch, chanIDB)

		// Validate batch when full.
		if len(batch) >= int(cfg.MaxBatchSize) {
			err := validateBatch()
			if err != nil {
				return fmt.Errorf("batch validation failed: %w",
					err)
			}
		}

		s.Do(func() {
			elapsed := time.Since(t0).Seconds()
			ratePerSec := float64(chunk) / elapsed
			log.Debugf("Migrated %d closed scids "+
				"(%.2f entries/sec)", count, ratePerSec)

			t0 = time.Now()
			chunk = 0
		})

		return nil
	}

	err := forEachClosedSCID(
		kvBackend, migrateSingleClosedSCID, func() {
			count = 0
			chunk = 0
			t0 = time.Now()
			batch = make([][]byte, 0, cfg.MaxBatchSize)
		},
	)
	if err != nil {
		return fmt.Errorf("could not migrate closed SCID index: %w",
			err)
	}

	// Validate any remaining SCIDs in the batch.
	if len(batch) > 0 {
		err := validateBatch()
		if err != nil {
			return fmt.Errorf("final batch validation failed: %w",
				err)
		}
	}

	log.Infof("Migrated %d closed SCIDs from KV to SQL in %s", count,
		time.Since(totalTime))

	return nil
}

// migrateZombieIndex migrates the zombie index from the KV backend to the SQL
// database. It collects zombie channels in batches, inserts them individually,
// and validates them in batches.
//
// NOTE: before inserting an entry into the zombie index, the function checks
// if the channel is already marked as closed in the SQL store. If it is,
// the entry is skipped. This means that the resulting zombie index count in
// the SQL store may well be less than the count of zombie channels in the KV
// store.
func migrateZombieIndex(ctx context.Context, cfg *sqldb.QueryConfig,
	kvBackend kvdb.Backend, sqlDB SQLQueries) error {

	var (
		totalTime = time.Now()

		count uint64

		t0    = time.Now()
		chunk uint64
		s     = rate.Sometimes{
			Interval: 10 * time.Second,
		}
	)

	type zombieEntry struct {
		pub1 route.Vertex
		pub2 route.Vertex
	}

	batch := make(map[uint64]*zombieEntry, cfg.MaxBatchSize)

	// validateBatch validates a batch of zombie SCIDs using batch query.
	validateBatch := func() error {
		if len(batch) == 0 {
			return nil
		}

		scids := make([][]byte, 0, len(batch))
		for scid := range batch {
			scids = append(scids, channelIDToBytes(scid))
		}

		// Batch fetch all zombie channels from the database.
		rows, err := sqlDB.GetZombieChannelsSCIDs(
			ctx, sqlc.GetZombieChannelsSCIDsParams{
				Version: int16(lnwire.GossipVersion1),
				Scids:   scids,
			},
		)
		if err != nil {
			return fmt.Errorf("could not batch get zombie "+
				"SCIDs: %w", err)
		}

		// Make sure that the number of rows returned matches
		// the number of SCIDs we requested.
		if len(rows) != len(scids) {
			return fmt.Errorf("expected to fetch %d zombie "+
				"SCIDs, but got %d", len(scids), len(rows))
		}

		// Validate each row is in the batch.
		for _, row := range rows {
			scid := byteOrder.Uint64(row.Scid)

			kvdbZombie, ok := batch[scid]
			if !ok {
				return fmt.Errorf("zombie SCID %x not found "+
					"in batch", scid)
			}

			err = sqldb.CompareRecords(
				kvdbZombie.pub1[:], row.NodeKey1,
				fmt.Sprintf("zombie pub key 1 (%s) for "+
					"channel %d", kvdbZombie.pub1, scid),
			)
			if err != nil {
				return err
			}

			err = sqldb.CompareRecords(
				kvdbZombie.pub2[:], row.NodeKey2,
				fmt.Sprintf("zombie pub key 2 (%s) for "+
					"channel %d", kvdbZombie.pub2, scid),
			)
			if err != nil {
				return err
			}
		}

		// Reset the batch for the next iteration.
		batch = make(map[uint64]*zombieEntry, cfg.MaxBatchSize)

		return nil
	}

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
		chunk++

		err = sqlDB.UpsertZombieChannel(
			ctx, sqlc.UpsertZombieChannelParams{
				Version:  int16(lnwire.GossipVersion1),
				Scid:     chanIDB,
				NodeKey1: pubKey1[:],
				NodeKey2: pubKey2[:],
			},
		)
		if err != nil {
			return fmt.Errorf("could not upsert zombie "+
				"channel %d: %w", chanID, err)
		}

		// Add to validation batch only after successful insertion.
		batch[chanID] = &zombieEntry{
			pub1: pubKey1,
			pub2: pubKey2,
		}

		// Validate batch when full.
		if len(batch) >= int(cfg.MaxBatchSize) {
			err := validateBatch()
			if err != nil {
				return fmt.Errorf("batch validation failed: %w",
					err)
			}
		}

		s.Do(func() {
			elapsed := time.Since(t0).Seconds()
			ratePerSec := float64(chunk) / elapsed
			log.Debugf("Migrated %d zombie index entries "+
				"(%.2f entries/sec)", count, ratePerSec)

			t0 = time.Now()
			chunk = 0
		})

		return nil
	}, func() {
		count = 0
		chunk = 0
		t0 = time.Now()
		batch = make(map[uint64]*zombieEntry, cfg.MaxBatchSize)
	})
	if err != nil {
		return fmt.Errorf("could not migrate zombie index: %w", err)
	}

	// Validate any remaining zombie SCIDs in the batch.
	if len(batch) > 0 {
		err := validateBatch()
		if err != nil {
			return fmt.Errorf("final batch validation failed: %w",
				err)
		}
	}

	log.Infof("Migrated %d zombie channels from KV to SQL in %s", count,
		time.Since(totalTime))

	return nil
}

// forEachZombieEntry iterates over each zombie channel entry in the
// KV backend and calls the provided callback function for each entry.
func forEachZombieEntry(db kvdb.Backend, cb func(chanID uint64, pubKey1,
	pubKey2 [33]byte) error, reset func()) error {

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
	}, reset)
}

// forEachClosedSCID iterates over each closed SCID in the KV backend and calls
// the provided callback function for each SCID.
func forEachClosedSCID(db kvdb.Backend,
	cb func(lnwire.ShortChannelID) error, reset func()) error {

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
	}, reset)
}

// insertNodeSQLMig inserts the node record into the database during the graph
// SQL migration. No error is expected if the node already exists. Unlike the
// main upsertNode function, this function does not require that a new node
// update have a newer timestamp than the existing one. This is because we want
// the migration to be idempotent and dont want to error out if we re-insert the
// exact same node.
func insertNodeSQLMig(ctx context.Context, db SQLQueries,
	node *models.Node) (int64, error) {

	params := sqlc.InsertNodeMigParams{
		Version: int16(lnwire.GossipVersion1),
		PubKey:  node.PubKeyBytes[:],
	}

	if node.HaveAnnouncement() {
		params.LastUpdate = sqldb.SQLInt64(node.LastUpdate.Unix())
		params.Color = sqldb.SQLStrValid(
			EncodeHexColor(node.Color.UnwrapOr(color.RGBA{})),
		)
		params.Alias = sqldb.SQLStrValid(node.Alias.UnwrapOr(""))
		params.Signature = node.AuthSigBytes
	}

	nodeID, err := db.InsertNodeMig(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("upserting node(%x): %w", node.PubKeyBytes,
			err)
	}

	// We can exit here if we don't have the announcement yet.
	if !node.HaveAnnouncement() {
		return nodeID, nil
	}

	// Insert the node's features.
	for feature := range node.Features.Features() {
		err = db.InsertNodeFeature(ctx, sqlc.InsertNodeFeatureParams{
			NodeID:     nodeID,
			FeatureBit: int32(feature),
		})
		if err != nil {
			return 0, fmt.Errorf("unable to insert node(%d) "+
				"feature(%v): %w", nodeID, feature, err)
		}
	}

	// Update the node's addresses.
	newAddresses, err := collectAddressRecords(node.Addresses)
	if err != nil {
		return 0, err
	}

	// Any remaining entries in newAddresses are new addresses that need to
	// be added to the database for the first time.
	for addrType, addrList := range newAddresses {
		for position, addr := range addrList {
			err := db.UpsertNodeAddress(
				ctx, sqlc.UpsertNodeAddressParams{
					NodeID:   nodeID,
					Type:     int16(addrType),
					Address:  addr,
					Position: int32(position),
				},
			)
			if err != nil {
				return 0, fmt.Errorf("unable to insert "+
					"node(%d) address(%v): %w", nodeID,
					addr, err)
			}
		}
	}

	// Convert the flat extra opaque data into a map of TLV types to
	// values.
	extra, err := marshalExtraOpaqueData(node.ExtraOpaqueData)
	if err != nil {
		return 0, fmt.Errorf("unable to marshal extra opaque data: %w",
			err)
	}

	// Insert the node's extra signed fields.
	for tlvType, value := range extra {
		err = db.UpsertNodeExtraType(
			ctx, sqlc.UpsertNodeExtraTypeParams{
				NodeID: nodeID,
				Type:   int64(tlvType),
				Value:  value,
			},
		)
		if err != nil {
			return 0, fmt.Errorf("unable to upsert node(%d) extra "+
				"signed field(%v): %w", nodeID, tlvType, err)
		}
	}

	return nodeID, nil
}

// dbChanInfo holds the DB level IDs of a channel and the nodes involved in the
// channel.
type dbChanInfo struct {
	channelID int64
	node1ID   int64
	node2ID   int64
}

// insertChannelMig inserts a new channel record into the database during the
// graph SQL migration.
func insertChannelMig(ctx context.Context, db SQLQueries,
	edge *models.ChannelEdgeInfo) (*dbChanInfo, error) {

	// Make sure that at least a "shell" entry for each node is present in
	// the nodes table.
	//
	// NOTE: we need this even during the SQL migration where nodes are
	// migrated first because there are cases were some nodes may have
	// been skipped due to invalid TLV data.
	node1DBID, err := maybeCreateShellNode(ctx, db, edge.NodeKey1Bytes)
	if err != nil {
		return nil, fmt.Errorf("unable to create shell node: %w", err)
	}

	node2DBID, err := maybeCreateShellNode(ctx, db, edge.NodeKey2Bytes)
	if err != nil {
		return nil, fmt.Errorf("unable to create shell node: %w", err)
	}

	var capacity sql.NullInt64
	if edge.Capacity != 0 {
		capacity = sqldb.SQLInt64(int64(edge.Capacity))
	}

	createParams := sqlc.InsertChannelMigParams{
		Version:     int16(lnwire.GossipVersion1),
		Scid:        channelIDToBytes(edge.ChannelID),
		NodeID1:     node1DBID,
		NodeID2:     node2DBID,
		Outpoint:    edge.ChannelPoint.String(),
		Capacity:    capacity,
		BitcoinKey1: edge.BitcoinKey1Bytes[:],
		BitcoinKey2: edge.BitcoinKey2Bytes[:],
	}

	if edge.AuthProof != nil {
		proof := edge.AuthProof

		createParams.Node1Signature = proof.NodeSig1Bytes
		createParams.Node2Signature = proof.NodeSig2Bytes
		createParams.Bitcoin1Signature = proof.BitcoinSig1Bytes
		createParams.Bitcoin2Signature = proof.BitcoinSig2Bytes
	}

	// Insert the new channel record.
	dbChanID, err := db.InsertChannelMig(ctx, createParams)
	if err != nil {
		return nil, err
	}

	// Insert any channel features.
	for feature := range edge.Features.Features() {
		err = db.InsertChannelFeature(
			ctx, sqlc.InsertChannelFeatureParams{
				ChannelID:  dbChanID,
				FeatureBit: int32(feature),
			},
		)
		if err != nil {
			return nil, fmt.Errorf("unable to insert channel(%d) "+
				"feature(%v): %w", dbChanID, feature, err)
		}
	}

	// Finally, insert any extra TLV fields in the channel announcement.
	extra, err := marshalExtraOpaqueData(edge.ExtraOpaqueData)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal extra opaque "+
			"data: %w", err)
	}

	for tlvType, value := range extra {
		err := db.UpsertChannelExtraType(
			ctx, sqlc.UpsertChannelExtraTypeParams{
				ChannelID: dbChanID,
				Type:      int64(tlvType),
				Value:     value,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("unable to upsert "+
				"channel(%d) extra signed field(%v): %w",
				edge.ChannelID, tlvType, err)
		}
	}

	return &dbChanInfo{
		channelID: dbChanID,
		node1ID:   node1DBID,
		node2ID:   node2DBID,
	}, nil
}

// insertChanEdgePolicyMig inserts the channel policy info we have stored for
// a channel we already know of. This is used during the SQL migration
// process to insert channel policies.
func insertChanEdgePolicyMig(ctx context.Context, tx SQLQueries,
	dbChan *dbChanInfo, edge *models.ChannelEdgePolicy) error {

	// Figure out which node this edge is from.
	isNode1 := edge.ChannelFlags&lnwire.ChanUpdateDirection == 0
	nodeID := dbChan.node1ID
	if !isNode1 {
		nodeID = dbChan.node2ID
	}

	var (
		inboundBase sql.NullInt64
		inboundRate sql.NullInt64
	)
	edge.InboundFee.WhenSome(func(fee lnwire.Fee) {
		inboundRate = sqldb.SQLInt64(fee.FeeRate)
		inboundBase = sqldb.SQLInt64(fee.BaseFee)
	})

	id, err := tx.InsertEdgePolicyMig(ctx, sqlc.InsertEdgePolicyMigParams{
		Version:     int16(lnwire.GossipVersion1),
		ChannelID:   dbChan.channelID,
		NodeID:      nodeID,
		Timelock:    int32(edge.TimeLockDelta),
		FeePpm:      int64(edge.FeeProportionalMillionths),
		BaseFeeMsat: int64(edge.FeeBaseMSat),
		MinHtlcMsat: int64(edge.MinHTLC),
		LastUpdate:  sqldb.SQLInt64(edge.LastUpdate.Unix()),
		Disabled: sql.NullBool{
			Valid: true,
			Bool:  edge.IsDisabled(),
		},
		MaxHtlcMsat: sql.NullInt64{
			Valid: edge.MessageFlags.HasMaxHtlc(),
			Int64: int64(edge.MaxHTLC),
		},
		MessageFlags:            sqldb.SQLInt16(edge.MessageFlags),
		ChannelFlags:            sqldb.SQLInt16(edge.ChannelFlags),
		InboundBaseFeeMsat:      inboundBase,
		InboundFeeRateMilliMsat: inboundRate,
		Signature:               edge.SigBytes,
	})
	if err != nil {
		return fmt.Errorf("unable to upsert edge policy: %w", err)
	}

	// Convert the flat extra opaque data into a map of TLV types to
	// values.
	extra, err := marshalExtraOpaqueData(edge.ExtraOpaqueData)
	if err != nil {
		return fmt.Errorf("unable to marshal extra opaque data: %w",
			err)
	}

	// Insert all new extra signed fields for the channel policy.
	for tlvType, value := range extra {
		err = tx.UpsertChanPolicyExtraType(
			ctx, sqlc.UpsertChanPolicyExtraTypeParams{
				ChannelPolicyID: id,
				Type:            int64(tlvType),
				Value:           value,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to insert "+
				"channel_policy(%d) extra signed field(%v): %w",
				id, tlvType, err)
		}
	}

	return nil
}
