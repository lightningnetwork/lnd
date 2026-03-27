package graphdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
)

// This file contains helpers for the private taproot V1→V2 migration shim.
//
// Private taproot channels were historically stored as V1 gossip objects with
// the SimpleTaprootChannelsRequiredStaging feature bit (180) set as a
// workaround. After the SQL migration upgrades these to canonical V2 storage,
// the shim projects them back to V1 for callers that are not yet V2-aware.
//
// When the gossiper and builder gain full V2 support (the g175-HEAD branch),
// this shim becomes dead code and can be removed.

const (
	// bitcoinGenesisTimestamp is the Unix timestamp of the Bitcoin genesis
	// block, used for rough timestamp↔block-height approximation.
	bitcoinGenesisTimestamp int64 = 1231006505

	// avgBlockInterval is the average time between Bitcoin blocks in
	// seconds, used for rough timestamp↔block-height approximation.
	avgBlockInterval int64 = 600
)

// timestampToApproxBlockHeight converts a Unix timestamp to an approximate
// Bitcoin block height. This is intentionally rough — it is only used for
// migrated private taproot channels where exact freshness ordering is not
// critical.
func timestampToApproxBlockHeight(t time.Time) uint32 {
	secs := t.Unix() - bitcoinGenesisTimestamp
	if secs <= 0 {
		return 0
	}

	return uint32(secs / avgBlockInterval)
}

// approxBlockHeightToTimestamp converts a block height back to an approximate
// Unix timestamp. This is the inverse of timestampToApproxBlockHeight.
func approxBlockHeightToTimestamp(h uint32) time.Time {
	return time.Unix(
		int64(h)*avgBlockInterval+bitcoinGenesisTimestamp, 0,
	)
}

// isPrivateTaprootV2 returns true if the given SQL channel row represents a
// private taproot channel that was migrated to V2 storage. In the near term,
// all V2 channels in the database are migrated private taproot channels since
// the gossiper does not yet create V2 channels. When full V2 gossip support
// lands, this function (and the entire shim) can be removed.
func isPrivateTaprootV2(ch sqlc.GraphChannel) bool {
	return ch.Version == int16(gossipV2) && len(ch.Signature) == 0
}

// projectV2EdgeToV1 takes a canonical V2 ChannelEdgeInfo (from a migrated
// private taproot channel) and projects it back to a V1 representation with
// the taproot staging feature bit re-added. This allows callers that only
// understand V1 to continue operating on the channel.
func projectV2EdgeToV1(
	edge *models.ChannelEdgeInfo) *models.ChannelEdgeInfo {

	// Build the V1 feature vector with the taproot staging bit.
	fv := lnwire.EmptyFeatureVector()
	if edge.Features != nil {
		for bit := range edge.Features.Features() {
			fv.Set(bit)
		}
	}
	fv.Set(lnwire.SimpleTaprootChannelsRequiredStaging)

	// Convert ExtraSignedFields back to ExtraOpaqueData.
	var extraOpaque []byte
	if len(edge.ExtraSignedFields) > 0 {
		recs, err := lnwire.CustomRecords(
			edge.ExtraSignedFields,
		).Serialize()
		if err == nil && recs != nil {
			extraOpaque = recs
		}
	}

	return &models.ChannelEdgeInfo{
		Version:          lnwire.GossipVersion1,
		ChannelID:        edge.ChannelID,
		ChainHash:        edge.ChainHash,
		NodeKey1Bytes:    edge.NodeKey1Bytes,
		NodeKey2Bytes:    edge.NodeKey2Bytes,
		BitcoinKey1Bytes: edge.BitcoinKey1Bytes,
		BitcoinKey2Bytes: edge.BitcoinKey2Bytes,
		Features:         fv,
		AuthProof:        nil, // Private channel.
		ChannelPoint:     edge.ChannelPoint,
		Capacity:         edge.Capacity,
		MerkleRootHash:   edge.MerkleRootHash,
		ExtraOpaqueData:  extraOpaque,
	}
}

// projectV2PolicyToV1 takes a canonical V2 ChannelEdgePolicy and projects it
// back to V1 representation. The block height is converted to an approximate
// timestamp, and the V2-specific direction/disable fields are mapped to V1
// ChannelFlags.
func projectV2PolicyToV1(
	p *models.ChannelEdgePolicy) *models.ChannelEdgePolicy {

	if p == nil {
		return nil
	}

	// Build V1 ChannelFlags from V2 direction and disable state.
	var chanFlags lnwire.ChanUpdateChanFlags
	if p.SecondPeer {
		chanFlags |= lnwire.ChanUpdateDirection
	}
	if !p.DisableFlags.IsEnabled() {
		chanFlags |= lnwire.ChanUpdateDisabled
	}

	// MaxHTLC is always present for V2, so set the message flag.
	msgFlags := lnwire.ChanUpdateRequiredMaxHtlc

	// Convert ExtraSignedFields back to ExtraOpaqueData.
	var extraOpaque lnwire.ExtraOpaqueData
	if len(p.ExtraSignedFields) > 0 {
		recs, err := lnwire.CustomRecords(
			p.ExtraSignedFields,
		).Serialize()
		if err == nil && recs != nil {
			extraOpaque = recs
		}
	}

	return &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion1,
		SigBytes:                  p.SigBytes,
		ChannelID:                 p.ChannelID,
		LastUpdate:                approxBlockHeightToTimestamp(p.LastBlockHeight), //nolint:ll
		MessageFlags:              msgFlags,
		ChannelFlags:              chanFlags,
		TimeLockDelta:             p.TimeLockDelta,
		MinHTLC:                   p.MinHTLC,
		MaxHTLC:                   p.MaxHTLC,
		FeeBaseMSat:               p.FeeBaseMSat,
		FeeProportionalMillionths: p.FeeProportionalMillionths,
		ToNode:                    p.ToNode,
		InboundFee:                p.InboundFee,
		ExtraOpaqueData:           extraOpaque,
	}
}

// projectV1PolicyToV2 takes a V1 ChannelEdgePolicy (from a caller updating a
// migrated private taproot channel) and converts it to V2 representation so it
// can be stored alongside the V2 channel row.
func projectV1PolicyToV2(
	p *models.ChannelEdgePolicy) *models.ChannelEdgePolicy {

	if p == nil {
		return nil
	}

	// Derive V2 direction from V1 ChannelFlags.
	secondPeer := p.ChannelFlags&lnwire.ChanUpdateDirection != 0

	// Derive V2 disable flags from V1 ChannelFlags.
	var disableFlags lnwire.ChanUpdateDisableFlags
	if p.ChannelFlags.IsDisabled() {
		disableFlags = lnwire.ChanUpdateDisableIncoming |
			lnwire.ChanUpdateDisableOutgoing
	}

	// Convert ExtraOpaqueData to ExtraSignedFields.
	var extraSigned map[uint64][]byte
	if len(p.ExtraOpaqueData) > 0 {
		m, err := marshalExtraOpaqueData(p.ExtraOpaqueData)
		if err == nil {
			extraSigned = m
		}
	}

	return &models.ChannelEdgePolicy{
		Version:                   lnwire.GossipVersion2,
		SigBytes:                  p.SigBytes,
		ChannelID:                 p.ChannelID,
		LastBlockHeight:           timestampToApproxBlockHeight(p.LastUpdate), //nolint:ll
		SecondPeer:                secondPeer,
		DisableFlags:              disableFlags,
		TimeLockDelta:             p.TimeLockDelta,
		MinHTLC:                   p.MinHTLC,
		MaxHTLC:                   p.MaxHTLC,
		FeeBaseMSat:               p.FeeBaseMSat,
		FeeProportionalMillionths: p.FeeProportionalMillionths,
		ToNode:                    p.ToNode,
		InboundFee:                p.InboundFee,
		ExtraSignedFields:         extraSigned,
	}
}

// projectV2EdgeInfoToV1 is a convenience wrapper for projection that also
// projects policies. It projects a V2 private taproot edge and its associated
// policies back to V1 representation.
func projectV2EdgeInfoToV1(edge *models.ChannelEdgeInfo,
	p1, p2 *models.ChannelEdgePolicy) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy) {

	return projectV2EdgeToV1(edge),
		projectV2PolicyToV1(p1),
		projectV2PolicyToV1(p2)
}

// isTaprootFeatureBit returns true if the given feature bit is one of the
// taproot staging feature bits used in the V1 workaround.
func isTaprootFeatureBit(bit int) bool {
	return bit == int(lnwire.SimpleTaprootChannelsRequiredStaging) ||
		bit == int(lnwire.SimpleTaprootChannelsOptionalStaging)
}

// featureVectorWithoutTaproot returns a new feature vector with the taproot
// staging bits removed. Used during migration to strip the workaround bits
// from the canonical V2 representation.
func featureVectorWithoutTaproot(
	fv *lnwire.FeatureVector) *lnwire.FeatureVector {

	if fv == nil {
		return lnwire.EmptyFeatureVector()
	}

	result := lnwire.EmptyFeatureVector()
	for bit := range fv.Features() {
		if !isTaprootFeatureBit(int(bit)) {
			result.Set(bit)
		}
	}

	return result
}

// v1EdgeIsPrivateTaproot checks if a V1 ChannelEdgeInfo is a private taproot
// workaround channel by examining the feature bits. Used to identify channels
// that need migration.
func v1EdgeIsPrivateTaproot(edge *models.ChannelEdgeInfo) bool {
	if edge.Version != lnwire.GossipVersion1 {
		return false
	}

	if edge.Features == nil {
		return false
	}

	return edge.Features.HasFeature(
		lnwire.SimpleTaprootChannelsOptionalStaging,
	)
}

// v2PrivateTaprootFallbackVersion returns gossipV2 if the requested version is
// gossipV1, indicating the shim should try a V2 fallback lookup. Returns
// fn.None if no fallback is applicable.
func v2PrivateTaprootFallbackVersion(
	v lnwire.GossipVersion) fn.Option[lnwire.GossipVersion] {

	if v == gossipV1 {
		return fn.Some(gossipV2)
	}

	return fn.None[lnwire.GossipVersion]()
}

// fetchV2TaprootFallbackBySCID attempts to find a V2 private taproot channel
// by SCID when a V1 lookup has failed. If found, it projects the result back
// to V1 and populates the output pointers. Returns ErrEdgeNotFound if no V2
// fallback exists.
//
// TODO(elle): remove when gossiper/builder are fully V2-aware.
func fetchV2TaprootFallbackBySCID(ctx context.Context, cfg *SQLStoreConfig,
	db SQLQueries, chanIDB []byte,
	edge **models.ChannelEdgeInfo,
	pol1, pol2 **models.ChannelEdgePolicy) error {

	row, err := db.GetChannelBySCIDWithPolicies(
		ctx, sqlc.GetChannelBySCIDWithPoliciesParams{
			Scid:    chanIDB,
			Version: int16(gossipV2),
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEdgeNotFound
	} else if err != nil {
		return fmt.Errorf("unable to fetch v2 fallback channel: %w",
			err)
	}

	// Only project private taproot V2 channels.
	if !isPrivateTaprootV2(row.GraphChannel) {
		return ErrEdgeNotFound
	}

	node1, node2, err := buildNodeVertices(
		row.GraphNode.PubKey, row.GraphNode_2.PubKey,
	)
	if err != nil {
		return err
	}

	v2Edge, err := getAndBuildEdgeInfo(
		ctx, cfg, db, row.GraphChannel, node1, node2,
	)
	if err != nil {
		return fmt.Errorf("unable to build v2 fallback channel "+
			"info: %w", err)
	}

	dbPol1, dbPol2, err := extractChannelPolicies(row)
	if err != nil {
		return fmt.Errorf("unable to extract v2 fallback "+
			"policies: %w", err)
	}

	v2Pol1, v2Pol2, err := getAndBuildChanPolicies(
		ctx, cfg.QueryCfg, db, dbPol1, dbPol2, v2Edge.ChannelID,
		node1, node2,
	)
	if err != nil {
		return fmt.Errorf("unable to build v2 fallback "+
			"policies: %w", err)
	}

	// Project back to V1.
	*edge, *pol1, *pol2 = projectV2EdgeInfoToV1(v2Edge, v2Pol1, v2Pol2)

	return nil
}

// fetchV2TaprootFallbackByOutpoint attempts to find a V2 private taproot
// channel by outpoint when a V1 lookup has failed. If found, it projects the
// result back to V1 and populates the output pointers. Returns ErrEdgeNotFound
// if no V2 fallback exists.
//
// TODO(elle): remove when gossiper/builder are fully V2-aware.
func fetchV2TaprootFallbackByOutpoint(ctx context.Context, cfg *SQLStoreConfig,
	db SQLQueries, outpoint string,
	edge **models.ChannelEdgeInfo,
	pol1, pol2 **models.ChannelEdgePolicy) error {

	row, err := db.GetChannelByOutpointWithPolicies(
		ctx, sqlc.GetChannelByOutpointWithPoliciesParams{
			Outpoint: outpoint,
			Version:  int16(gossipV2),
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEdgeNotFound
	} else if err != nil {
		return fmt.Errorf("unable to fetch v2 fallback channel: %w",
			err)
	}

	// Only project private taproot V2 channels.
	if !isPrivateTaprootV2(row.GraphChannel) {
		return ErrEdgeNotFound
	}

	node1, node2, err := buildNodeVertices(
		row.Node1Pubkey, row.Node2Pubkey,
	)
	if err != nil {
		return err
	}

	v2Edge, err := getAndBuildEdgeInfo(
		ctx, cfg, db, row.GraphChannel, node1, node2,
	)
	if err != nil {
		return fmt.Errorf("unable to build v2 fallback channel "+
			"info: %w", err)
	}

	dbPol1, dbPol2, err := extractChannelPolicies(row)
	if err != nil {
		return fmt.Errorf("unable to extract v2 fallback "+
			"policies: %w", err)
	}

	v2Pol1, v2Pol2, err := getAndBuildChanPolicies(
		ctx, cfg.QueryCfg, db, dbPol1, dbPol2, v2Edge.ChannelID,
		node1, node2,
	)
	if err != nil {
		return fmt.Errorf("unable to build v2 fallback "+
			"policies: %w", err)
	}

	// Project back to V1.
	*edge, *pol1, *pol2 = projectV2EdgeInfoToV1(v2Edge, v2Pol1, v2Pol2)

	return nil
}

// appendV2TaprootEdgePoints iterates V2 channels and appends EdgePoints for
// any private taproot channels to the given slice. This is used by ChannelView
// in the V1 path to include migrated private taproot channels in the startup
// chain filter.
//
// TODO(elle): remove when gossiper/builder are fully V2-aware.
func appendV2TaprootEdgePoints(ctx context.Context,
	queryCfg *sqldb.QueryConfig, db SQLQueries,
	edgePoints *[]EdgePoint) error {

	handleChannel := func(_ context.Context,
		channel sqlc.ListChannelsPaginatedV2Row) error {

		op, err := wire.NewOutPointFromString(channel.Outpoint)
		if err != nil {
			return err
		}

		*edgePoints = append(*edgePoints, EdgePoint{
			FundingPkScript: channel.FundingPkScript,
			OutPoint:        *op,
		})

		return nil
	}

	queryFunc := func(ctx context.Context, lastID int64,
		limit int32) ([]sqlc.ListChannelsPaginatedV2Row, error) {

		return db.ListChannelsPaginatedV2(
			ctx, sqlc.ListChannelsPaginatedV2Params{
				ID:    lastID,
				Limit: limit,
			},
		)
	}

	extractCursor := func(
		row sqlc.ListChannelsPaginatedV2Row) int64 {

		return row.ID
	}

	return sqldb.ExecutePaginatedQuery(
		ctx, queryCfg, int64(-1), queryFunc,
		extractCursor, handleChannel,
	)
}
