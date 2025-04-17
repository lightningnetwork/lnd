package graphdb

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

func MigrateGraphToSQL(ctx context.Context, kvStore *KVStore,
	tx *sqlc.Queries) error {

	sqlDB := SQLQueries(tx)

	// First, migrate all nodes.
	err := kvStore.ForEachNode(func(nodeTx NodeRTx) error {
		_, err := upsertNode(ctx, sqlDB, nodeTx.Node())
		if err != nil {
			return fmt.Errorf("could not migrate node(%x): %w",
				nodeTx.Node().PubKeyBytes, err)
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("could not migrate nodes: %w", err)
	}

	migrateChanPolicy := func(dbInfo *dbChanInfo,
		policy *models.ChannelEdgePolicy) error {

		if policy == nil {
			return nil
		}

		_, _, _, err := updateChanEdgePolicy(ctx, sqlDB, policy)

		return err
	}

	// Next, migrate all channels and channel updates
	err = kvStore.ForEachChannel(
		func(info *models.ChannelEdgeInfo,
			policy1 *models.ChannelEdgePolicy,
			policy2 *models.ChannelEdgePolicy) error {

			dbChanInfo, err := insertChannel(ctx, sqlDB, info)
			if err != nil {
				return fmt.Errorf("could not migrate "+
					"channel(%d): %w", info.ChannelID, err)
			}

			err = migrateChanPolicy(dbChanInfo, policy1)
			if err != nil {
				return fmt.Errorf("could not migrate "+
					"policy1(%d): %w", info.ChannelID, err)
			}

			err = migrateChanPolicy(dbChanInfo, policy2)
			if err != nil {
				return fmt.Errorf("could not migrate "+
					"policy2(%d): %w", info.ChannelID, err)
			}

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("could not migrate channels and channel "+
			"updates: %w", err)
	}

	// Migrate source node.
	err = migrateSourceNode(ctx, kvStore, sqlDB)
	if err != nil {
		return fmt.Errorf("could not migrate source node: %w", err)
	}

	// Migrate prune log.
	err = kvStore.forEachPruneLogEntry(
		func(height uint32, hash *chainhash.Hash) error {
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

			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("could not migrate prune log: %w", err)
	}

	// Migrate closed SCID index.
	err = kvStore.forEachClosedSCID(func(scid lnwire.ShortChannelID) error {
		var chanIDB [8]byte
		byteOrder.PutUint64(chanIDB[:], scid.ToUint64())

		return sqlDB.InsertClosedChannel(ctx, chanIDB[:])
	})
	if err != nil {
		return fmt.Errorf("could not migrate closed SCID index: %w",
			err)
	}

	// Migrate zombie index.
	err = kvStore.forEachZombieEntry(func(chanID uint64, pubKey1,
		pubKey2 [33]byte) error {

		var chanIDB [8]byte
		byteOrder.PutUint64(chanIDB[:], chanID)

		// If it is in the closed SCID index, we don't need to
		// add it to the zombie index.
		isClosed, err := sqlDB.IsClosedChannel(ctx, chanIDB[:])
		if err != nil {
			return fmt.Errorf("could not check closed "+
				"channel: %w", err)
		}

		if isClosed {
			return nil
		}

		return sqlDB.UpsertZombieChannel(
			ctx, sqlc.UpsertZombieChannelParams{
				Version:  int16(ProtocolV1),
				Scid:     chanIDB[:],
				NodeKey1: pubKey1[:],
				NodeKey2: pubKey2[:],
			},
		)
	})
	if err != nil {
		return fmt.Errorf("could not migrate zombie index: %w", err)
	}

	return nil
}

func migrateSourceNode(ctx context.Context, kvStore *KVStore,
	sqlDB SQLQueries) error {

	sourceNode, err := kvStore.SourceNode(ctx)
	if errors.Is(err, ErrSourceNodeNotSet) {
		return nil
	} else if err != nil {
		return fmt.Errorf("could not get source node: %w", err)
	}

	dbSourceNode, err := sqlDB.GetNodeByPubKey(
		ctx, sqlc.GetNodeByPubKeyParams{
			PubKey:  sourceNode.PubKeyBytes[:],
			Version: int16(ProtocolV1),
		},
	)
	if err != nil {
		return fmt.Errorf("could not get source node ID: %w", err)
	}

	err = sqlDB.AddSourceNode(ctx, dbSourceNode.ID)
	if err != nil {
		return fmt.Errorf("could not add source node: %w", err)
	}

	return nil
}
