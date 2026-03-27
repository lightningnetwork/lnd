package graphdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// taprootMigrationCandidate holds a V1 channel with its node pubkeys for
// migration to V2.
type taprootMigrationCandidate struct {
	channel     sqlc.GraphChannel
	node1PubKey []byte
	node2PubKey []byte
}

// MigratePrivateTaprootToV2 migrates private taproot channels from the V1
// workaround storage format (V1 channel with SimpleTaprootChannelsStaging
// feature bit) to canonical V2 storage. This is the entry point used by the
// migration framework via config_builder.
//
// The migration:
//   - Finds V1 channels with the taproot staging feature bit
//   - Creates V2 shell nodes for both channel endpoints
//   - Computes and stores the taproot funding script
//   - Converts policies from timestamp-based to block-height-based
//   - Copies features (minus the taproot staging bit), extras, and policies
//   - Deletes the old V1 rows (CASCADE cleans up dependent rows)
//
// The migration is idempotent: after running, there are no more V1 channels
// with the taproot staging feature bit, so re-running is a no-op.
func MigratePrivateTaprootToV2(ctx context.Context,
	tx *sqlc.Queries) error {

	return migratePrivateTaprootToV2(ctx, tx)
}

// RunTaprootV2Migration runs the private taproot V1→V2 migration within the
// SQL store's transaction context. This is used in tests to run the migration
// against a test store.
func (s *SQLStore) RunTaprootV2Migration(ctx context.Context) error {
	return s.db.ExecTx(
		ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
			return migratePrivateTaprootToV2(ctx, db)
		}, sqldb.NoOpReset,
	)
}

// migratePrivateTaprootToV2 is the internal implementation that works with
// any type implementing the SQLQueries interface (which both *sqlc.Queries
// and the transaction wrapper satisfy).
func migratePrivateTaprootToV2(ctx context.Context,
	tx SQLQueries) error {

	log.Infof("Starting private taproot V1→V2 migration")
	t0 := time.Now()

	candidates, err := findTaprootV1Channels(ctx, tx)
	if err != nil {
		return fmt.Errorf("finding taproot v1 channels: %w", err)
	}

	if len(candidates) == 0 {
		log.Infof("No private taproot V1 channels found, skipping")

		return nil
	}

	log.Infof("Found %d private taproot V1 channel(s) to migrate",
		len(candidates))

	for _, c := range candidates {
		if err := migrateOneChannel(ctx, tx, c); err != nil {
			return fmt.Errorf("migrating channel %x: %w",
				c.channel.Scid, err)
		}
	}

	log.Infof("Finished private taproot V1→V2 migration of %d "+
		"channel(s) in %v", len(candidates), time.Since(t0))

	return nil
}

// findTaprootV1Channels returns all V1 channels that have the taproot staging
// feature bit set, along with their node pubkeys.
func findTaprootV1Channels(ctx context.Context,
	tx SQLQueries) ([]taprootMigrationCandidate, error) {

	var result []taprootMigrationCandidate

	// Iterate V1 channels in pages using the full-row query which
	// includes node pubkeys.
	var lastID int64 = -1
	for {
		rows, err := tx.ListChannelsWithPoliciesPaginated(
			ctx, sqlc.ListChannelsWithPoliciesPaginatedParams{
				Version: int16(gossipV1),
				ID:      lastID,
				Limit:   100,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("listing channels: %w", err)
		}

		if len(rows) == 0 {
			break
		}

		// Collect DB IDs for batch feature lookup.
		ids := make([]int64, 0, len(rows))
		idToRow := make(
			map[int64]sqlc.ListChannelsWithPoliciesPaginatedRow,
			len(rows),
		)
		for _, row := range rows {
			ids = append(ids, row.GraphChannel.ID)
			idToRow[row.GraphChannel.ID] = row
			lastID = row.GraphChannel.ID
		}

		// Batch load features for these channels.
		features, err := tx.GetChannelFeaturesBatch(ctx, ids)
		if err != nil {
			return nil, fmt.Errorf("fetching features: %w", err)
		}

		// Find channels with the taproot staging bit.
		seen := make(map[int64]bool)
		for _, f := range features {
			if !isTaprootFeatureBit(int(f.FeatureBit)) {
				continue
			}

			if seen[f.ChannelID] {
				continue
			}
			seen[f.ChannelID] = true

			row, ok := idToRow[f.ChannelID]
			if !ok {
				continue
			}

			result = append(result, taprootMigrationCandidate{
				channel:     row.GraphChannel,
				node1PubKey: row.Node1Pubkey,
				node2PubKey: row.Node2Pubkey,
			})
		}
	}

	return result, nil
}

// migrateOneChannel migrates a single private taproot V1 channel to V2.
func migrateOneChannel(ctx context.Context, tx SQLQueries,
	c taprootMigrationCandidate) error {

	ch := c.channel

	// 1. Parse bitcoin keys and compute taproot funding script.
	pubKey1, err := btcec.ParsePubKey(ch.BitcoinKey1)
	if err != nil {
		return fmt.Errorf("parsing bitcoin key 1: %w", err)
	}

	pubKey2, err := btcec.ParsePubKey(ch.BitcoinKey2)
	if err != nil {
		return fmt.Errorf("parsing bitcoin key 2: %w", err)
	}

	var merkleRoot fn.Option[chainhash.Hash]
	if len(ch.MerkleRootHash) > 0 {
		var hash chainhash.Hash
		copy(hash[:], ch.MerkleRootHash)
		merkleRoot = fn.Some(hash)
	}

	fundingScript, _, err := input.GenTaprootFundingScript(
		pubKey1, pubKey2, 0, merkleRoot,
	)
	if err != nil {
		return fmt.Errorf("generating funding script: %w", err)
	}

	// 2. Create V2 shell nodes (or get existing V2 node IDs).
	v2Node1ID, err := tx.UpsertNode(ctx, sqlc.UpsertNodeParams{
		Version: int16(gossipV2),
		PubKey:  c.node1PubKey,
	})
	if err != nil {
		return fmt.Errorf("creating v2 shell node 1: %w", err)
	}

	v2Node2ID, err := tx.UpsertNode(ctx, sqlc.UpsertNodeParams{
		Version: int16(gossipV2),
		PubKey:  c.node2PubKey,
	})
	if err != nil {
		return fmt.Errorf("creating v2 shell node 2: %w", err)
	}

	// 3. Create V2 channel row.
	v2ChanID, err := tx.CreateChannel(ctx, sqlc.CreateChannelParams{
		Version:         int16(gossipV2),
		Scid:            ch.Scid,
		NodeID1:         v2Node1ID,
		NodeID2:         v2Node2ID,
		Outpoint:        ch.Outpoint,
		Capacity:        ch.Capacity,
		BitcoinKey1:     ch.BitcoinKey1,
		BitcoinKey2:     ch.BitcoinKey2,
		FundingPkScript: fundingScript,
		MerkleRootHash:  ch.MerkleRootHash,
	})
	if err != nil {
		return fmt.Errorf("creating v2 channel: %w", err)
	}

	// 4. Copy features (excluding taproot staging bits).
	v1Features, err := tx.GetChannelFeaturesBatch(ctx, []int64{ch.ID})
	if err != nil {
		return fmt.Errorf("fetching v1 features: %w", err)
	}

	for _, f := range v1Features {
		if isTaprootFeatureBit(int(f.FeatureBit)) {
			continue
		}

		err := tx.InsertChannelFeature(
			ctx, sqlc.InsertChannelFeatureParams{
				ChannelID:  v2ChanID,
				FeatureBit: f.FeatureBit,
			},
		)
		if err != nil {
			return fmt.Errorf("copying feature %d: %w",
				f.FeatureBit, err)
		}
	}

	// 5. Copy channel extra types.
	v1Extras, err := tx.GetChannelExtrasBatch(ctx, []int64{ch.ID})
	if err != nil {
		return fmt.Errorf("fetching v1 channel extras: %w", err)
	}

	for _, extra := range v1Extras {
		err := tx.UpsertChannelExtraType(
			ctx, sqlc.UpsertChannelExtraTypeParams{
				ChannelID: v2ChanID,
				Type:      extra.Type,
				Value:     extra.Value,
			},
		)
		if err != nil {
			return fmt.Errorf("copying channel extra: %w", err)
		}
	}

	// 6. Convert and copy policies.
	if err := migratePolicies(ctx, tx, ch, c, v2ChanID); err != nil {
		return fmt.Errorf("migrating policies: %w", err)
	}

	// 7. Delete old V1 channel (CASCADE handles features, extras,
	//    policies, policy extras).
	err = tx.DeleteChannels(ctx, []int64{ch.ID})
	if err != nil {
		return fmt.Errorf("deleting v1 channel: %w", err)
	}

	return nil
}

// migratePolicies converts V1 policies for a channel to V2 and inserts them.
func migratePolicies(ctx context.Context, tx SQLQueries,
	v1Chan sqlc.GraphChannel, c taprootMigrationCandidate,
	v2ChanID int64) error {

	// Map V1 node IDs to V2 node IDs via pubkey lookup. We already
	// created the V2 shell nodes in migrateOneChannel.
	v1NodeIDToV2 := make(map[int64]int64)
	for _, entry := range []struct {
		v1NodeID int64
		pubKey   []byte
	}{
		{v1Chan.NodeID1, c.node1PubKey},
		{v1Chan.NodeID2, c.node2PubKey},
	} {
		v2ID, err := tx.GetNodeIDByPubKey(
			ctx, sqlc.GetNodeIDByPubKeyParams{
				Version: int16(gossipV2),
				PubKey:  entry.pubKey,
			},
		)
		if err != nil {
			return fmt.Errorf("fetching v2 node ID: %w", err)
		}

		v1NodeIDToV2[entry.v1NodeID] = v2ID
	}

	// Fetch and convert policies for both directions.
	for _, nodeID := range []int64{v1Chan.NodeID1, v1Chan.NodeID2} {
		policy, err := tx.GetChannelPolicyByChannelAndNode(
			ctx, sqlc.GetChannelPolicyByChannelAndNodeParams{
				Version:   int16(gossipV1),
				ChannelID: v1Chan.ID,
				NodeID:    nodeID,
			},
		)
		if err != nil {
			if err == sql.ErrNoRows {
				continue
			}

			return fmt.Errorf("fetching v1 policy: %w", err)
		}

		// Convert timestamp to approximate block height.
		var blockHeight sql.NullInt64
		if policy.LastUpdate.Valid {
			ts := time.Unix(policy.LastUpdate.Int64, 0)
			h := timestampToApproxBlockHeight(ts)
			blockHeight = sqldb.SQLInt64(int64(h))
		}

		// Convert disabled bool to disable flags.
		var disableFlags sql.NullInt16
		if policy.Disabled.Valid && policy.Disabled.Bool {
			disableFlags = sqldb.SQLInt16(
				lnwire.ChanUpdateDisableIncoming |
					lnwire.ChanUpdateDisableOutgoing,
			)
		} else {
			disableFlags = sqldb.SQLInt16(0)
		}

		v2NodeID := v1NodeIDToV2[nodeID]

		v2PolicyID, err := tx.UpsertEdgePolicy(
			ctx, sqlc.UpsertEdgePolicyParams{
				Version:                 int16(gossipV2),
				ChannelID:               v2ChanID,
				NodeID:                  v2NodeID,
				Timelock:                policy.Timelock,
				FeePpm:                  policy.FeePpm,
				BaseFeeMsat:             policy.BaseFeeMsat,
				MinHtlcMsat:             policy.MinHtlcMsat,
				MaxHtlcMsat:             policy.MaxHtlcMsat,
				BlockHeight:             blockHeight,
				DisableFlags:            disableFlags,
				InboundBaseFeeMsat:      policy.InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: policy.InboundFeeRateMilliMsat, //nolint:ll
				Signature:               policy.Signature,
			},
		)
		if err != nil {
			return fmt.Errorf("upserting v2 policy: %w", err)
		}

		// Copy policy extra types.
		policyExtras, err := tx.GetChannelPolicyExtraTypesBatch(
			ctx, []int64{policy.ID},
		)
		if err != nil {
			return fmt.Errorf("fetching policy extras: %w", err)
		}

		for _, extra := range policyExtras {
			err := tx.UpsertChanPolicyExtraType(
				ctx, sqlc.UpsertChanPolicyExtraTypeParams{
					ChannelPolicyID: v2PolicyID,
					Type:            extra.Type,
					Value:           extra.Value,
				},
			)
			if err != nil {
				return fmt.Errorf("copying policy extra: %w",
					err)
			}
		}
	}

	return nil
}
