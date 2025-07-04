package graphdb

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/kvdb"
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
	sqlDB SQLQueries, _ chainhash.Hash) error {

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
	})
	if err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	log.Infof("Migrated %d nodes from KV to SQL (skipped %d nodes due to "+
		"invalid TLV streams)", count, skipped)

	return nil
}
