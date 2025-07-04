package graphdb

import (
	"context"
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

	// 2) Migrate the source node.
	if err := migrateSourceNode(ctx, kvBackend, sqlDB); err != nil {
		return fmt.Errorf("could not migrate source node: %w", err)
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
