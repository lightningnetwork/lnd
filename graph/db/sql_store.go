package graphdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	color "image/color"
	"iter"
	"maps"
	"math"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/aliasmgr"
	"github.com/lightningnetwork/lnd/batch"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
	"github.com/lightningnetwork/lnd/sqldb/sqlc"
	"github.com/lightningnetwork/lnd/tlv"
	"github.com/lightningnetwork/lnd/tor"
)

// SQLQueries is a subset of the sqlc.Querier interface that can be used to
// execute queries against the SQL graph tables.
//
//nolint:ll,interfacebloat
type SQLQueries interface {
	/*
		Node queries.
	*/
	UpsertNode(ctx context.Context, arg sqlc.UpsertNodeParams) (int64, error)
	UpsertSourceNode(ctx context.Context, arg sqlc.UpsertSourceNodeParams) (int64, error)
	GetNodeByPubKey(ctx context.Context, arg sqlc.GetNodeByPubKeyParams) (sqlc.GraphNode, error)
	GetNodesByIDs(ctx context.Context, ids []int64) ([]sqlc.GraphNode, error)
	GetNodeIDByPubKey(ctx context.Context, arg sqlc.GetNodeIDByPubKeyParams) (int64, error)
	GetNodesByLastUpdateRange(ctx context.Context, arg sqlc.GetNodesByLastUpdateRangeParams) ([]sqlc.GraphNode, error)
	ListNodesPaginated(ctx context.Context, arg sqlc.ListNodesPaginatedParams) ([]sqlc.GraphNode, error)
	ListNodeIDsAndPubKeys(ctx context.Context, arg sqlc.ListNodeIDsAndPubKeysParams) ([]sqlc.ListNodeIDsAndPubKeysRow, error)
	IsPublicV1Node(ctx context.Context, pubKey []byte) (bool, error)
	DeleteUnconnectedNodes(ctx context.Context) ([][]byte, error)
	DeleteNodeByPubKey(ctx context.Context, arg sqlc.DeleteNodeByPubKeyParams) (sql.Result, error)
	DeleteNode(ctx context.Context, id int64) error

	GetExtraNodeTypes(ctx context.Context, nodeID int64) ([]sqlc.GraphNodeExtraType, error)
	GetNodeExtraTypesBatch(ctx context.Context, ids []int64) ([]sqlc.GraphNodeExtraType, error)
	UpsertNodeExtraType(ctx context.Context, arg sqlc.UpsertNodeExtraTypeParams) error
	DeleteExtraNodeType(ctx context.Context, arg sqlc.DeleteExtraNodeTypeParams) error

	UpsertNodeAddress(ctx context.Context, arg sqlc.UpsertNodeAddressParams) error
	GetNodeAddresses(ctx context.Context, nodeID int64) ([]sqlc.GetNodeAddressesRow, error)
	GetNodeAddressesBatch(ctx context.Context, ids []int64) ([]sqlc.GraphNodeAddress, error)
	DeleteNodeAddresses(ctx context.Context, nodeID int64) error

	InsertNodeFeature(ctx context.Context, arg sqlc.InsertNodeFeatureParams) error
	GetNodeFeatures(ctx context.Context, nodeID int64) ([]sqlc.GraphNodeFeature, error)
	GetNodeFeaturesBatch(ctx context.Context, ids []int64) ([]sqlc.GraphNodeFeature, error)
	GetNodeFeaturesByPubKey(ctx context.Context, arg sqlc.GetNodeFeaturesByPubKeyParams) ([]int32, error)
	DeleteNodeFeature(ctx context.Context, arg sqlc.DeleteNodeFeatureParams) error

	/*
		Source node queries.
	*/
	AddSourceNode(ctx context.Context, nodeID int64) error
	GetSourceNodesByVersion(ctx context.Context, version int16) ([]sqlc.GetSourceNodesByVersionRow, error)

	/*
		Channel queries.
	*/
	CreateChannel(ctx context.Context, arg sqlc.CreateChannelParams) (int64, error)
	AddV1ChannelProof(ctx context.Context, arg sqlc.AddV1ChannelProofParams) (sql.Result, error)
	GetChannelBySCID(ctx context.Context, arg sqlc.GetChannelBySCIDParams) (sqlc.GraphChannel, error)
	GetChannelsBySCIDs(ctx context.Context, arg sqlc.GetChannelsBySCIDsParams) ([]sqlc.GraphChannel, error)
	GetChannelsByOutpoints(ctx context.Context, outpoints []string) ([]sqlc.GetChannelsByOutpointsRow, error)
	GetChannelsBySCIDRange(ctx context.Context, arg sqlc.GetChannelsBySCIDRangeParams) ([]sqlc.GetChannelsBySCIDRangeRow, error)
	GetChannelBySCIDWithPolicies(ctx context.Context, arg sqlc.GetChannelBySCIDWithPoliciesParams) (sqlc.GetChannelBySCIDWithPoliciesRow, error)
	GetChannelsBySCIDWithPolicies(ctx context.Context, arg sqlc.GetChannelsBySCIDWithPoliciesParams) ([]sqlc.GetChannelsBySCIDWithPoliciesRow, error)
	GetChannelsByIDs(ctx context.Context, ids []int64) ([]sqlc.GetChannelsByIDsRow, error)
	GetChannelAndNodesBySCID(ctx context.Context, arg sqlc.GetChannelAndNodesBySCIDParams) (sqlc.GetChannelAndNodesBySCIDRow, error)
	HighestSCID(ctx context.Context, version int16) ([]byte, error)
	ListChannelsByNodeID(ctx context.Context, arg sqlc.ListChannelsByNodeIDParams) ([]sqlc.ListChannelsByNodeIDRow, error)
	ListChannelsForNodeIDs(ctx context.Context, arg sqlc.ListChannelsForNodeIDsParams) ([]sqlc.ListChannelsForNodeIDsRow, error)
	ListChannelsWithPoliciesPaginated(ctx context.Context, arg sqlc.ListChannelsWithPoliciesPaginatedParams) ([]sqlc.ListChannelsWithPoliciesPaginatedRow, error)
	ListChannelsWithPoliciesForCachePaginated(ctx context.Context, arg sqlc.ListChannelsWithPoliciesForCachePaginatedParams) ([]sqlc.ListChannelsWithPoliciesForCachePaginatedRow, error)
	ListChannelsPaginated(ctx context.Context, arg sqlc.ListChannelsPaginatedParams) ([]sqlc.ListChannelsPaginatedRow, error)
	GetChannelsByPolicyLastUpdateRange(ctx context.Context, arg sqlc.GetChannelsByPolicyLastUpdateRangeParams) ([]sqlc.GetChannelsByPolicyLastUpdateRangeRow, error)
	GetChannelByOutpointWithPolicies(ctx context.Context, arg sqlc.GetChannelByOutpointWithPoliciesParams) (sqlc.GetChannelByOutpointWithPoliciesRow, error)
	GetPublicV1ChannelsBySCID(ctx context.Context, arg sqlc.GetPublicV1ChannelsBySCIDParams) ([]sqlc.GraphChannel, error)
	GetSCIDByOutpoint(ctx context.Context, arg sqlc.GetSCIDByOutpointParams) ([]byte, error)
	DeleteChannels(ctx context.Context, ids []int64) error

	UpsertChannelExtraType(ctx context.Context, arg sqlc.UpsertChannelExtraTypeParams) error
	GetChannelExtrasBatch(ctx context.Context, chanIds []int64) ([]sqlc.GraphChannelExtraType, error)
	InsertChannelFeature(ctx context.Context, arg sqlc.InsertChannelFeatureParams) error
	GetChannelFeaturesBatch(ctx context.Context, chanIds []int64) ([]sqlc.GraphChannelFeature, error)

	/*
		Channel Policy table queries.
	*/
	UpsertEdgePolicy(ctx context.Context, arg sqlc.UpsertEdgePolicyParams) (int64, error)
	GetChannelPolicyByChannelAndNode(ctx context.Context, arg sqlc.GetChannelPolicyByChannelAndNodeParams) (sqlc.GraphChannelPolicy, error)
	GetV1DisabledSCIDs(ctx context.Context) ([][]byte, error)

	UpsertChanPolicyExtraType(ctx context.Context, arg sqlc.UpsertChanPolicyExtraTypeParams) error
	GetChannelPolicyExtraTypesBatch(ctx context.Context, policyIds []int64) ([]sqlc.GetChannelPolicyExtraTypesBatchRow, error)
	DeleteChannelPolicyExtraTypes(ctx context.Context, channelPolicyID int64) error

	/*
		Zombie index queries.
	*/
	UpsertZombieChannel(ctx context.Context, arg sqlc.UpsertZombieChannelParams) error
	GetZombieChannel(ctx context.Context, arg sqlc.GetZombieChannelParams) (sqlc.GraphZombieChannel, error)
	GetZombieChannelsSCIDs(ctx context.Context, arg sqlc.GetZombieChannelsSCIDsParams) ([]sqlc.GraphZombieChannel, error)
	CountZombieChannels(ctx context.Context, version int16) (int64, error)
	DeleteZombieChannel(ctx context.Context, arg sqlc.DeleteZombieChannelParams) (sql.Result, error)
	IsZombieChannel(ctx context.Context, arg sqlc.IsZombieChannelParams) (bool, error)

	/*
		Prune log table queries.
	*/
	GetPruneTip(ctx context.Context) (sqlc.GraphPruneLog, error)
	GetPruneHashByHeight(ctx context.Context, blockHeight int64) ([]byte, error)
	GetPruneEntriesForHeights(ctx context.Context, heights []int64) ([]sqlc.GraphPruneLog, error)
	UpsertPruneLogEntry(ctx context.Context, arg sqlc.UpsertPruneLogEntryParams) error
	DeletePruneLogEntriesInRange(ctx context.Context, arg sqlc.DeletePruneLogEntriesInRangeParams) error

	/*
		Closed SCID table queries.
	*/
	InsertClosedChannel(ctx context.Context, scid []byte) error
	IsClosedChannel(ctx context.Context, scid []byte) (bool, error)
	GetClosedChannelsSCIDs(ctx context.Context, scids [][]byte) ([][]byte, error)

	/*
		Migration specific queries.

		NOTE: these should not be used in code other than migrations.
		Once sqldbv2 is in place, these can be removed from this struct
		as then migrations will have their own dedicated queries
		structs.
	*/
	InsertNodeMig(ctx context.Context, arg sqlc.InsertNodeMigParams) (int64, error)
	InsertChannelMig(ctx context.Context, arg sqlc.InsertChannelMigParams) (int64, error)
	InsertEdgePolicyMig(ctx context.Context, arg sqlc.InsertEdgePolicyMigParams) (int64, error)
}

// BatchedSQLQueries is a version of SQLQueries that's capable of batched
// database operations.
type BatchedSQLQueries interface {
	SQLQueries
	sqldb.BatchedTx[SQLQueries]
}

// SQLStore is an implementation of the V1Store interface that uses a SQL
// database as the backend.
type SQLStore struct {
	cfg *SQLStoreConfig
	db  BatchedSQLQueries

	// cacheMu guards all caches (rejectCache and chanCache). If
	// this mutex will be acquired at the same time as the DB mutex then
	// the cacheMu MUST be acquired first to prevent deadlock.
	cacheMu     sync.RWMutex
	rejectCache *rejectCache
	chanCache   *channelCache

	chanScheduler batch.Scheduler[SQLQueries]
	nodeScheduler batch.Scheduler[SQLQueries]

	srcNodes  map[lnwire.GossipVersion]*srcNodeInfo
	srcNodeMu sync.Mutex
}

// A compile-time assertion to ensure that SQLStore implements the V1Store
// interface.
var _ V1Store = (*SQLStore)(nil)

// SQLStoreConfig holds the configuration for the SQLStore.
type SQLStoreConfig struct {
	// ChainHash is the genesis hash for the chain that all the gossip
	// messages in this store are aimed at.
	ChainHash chainhash.Hash

	// QueryConfig holds configuration values for SQL queries.
	QueryCfg *sqldb.QueryConfig
}

// NewSQLStore creates a new SQLStore instance given an open BatchedSQLQueries
// storage backend.
func NewSQLStore(cfg *SQLStoreConfig, db BatchedSQLQueries,
	options ...StoreOptionModifier) (*SQLStore, error) {

	opts := DefaultOptions()
	for _, o := range options {
		o(opts)
	}

	if opts.NoMigration {
		return nil, fmt.Errorf("the NoMigration option is not yet " +
			"supported for SQL stores")
	}

	s := &SQLStore{
		cfg:         cfg,
		db:          db,
		rejectCache: newRejectCache(opts.RejectCacheSize),
		chanCache:   newChannelCache(opts.ChannelCacheSize),
		srcNodes:    make(map[lnwire.GossipVersion]*srcNodeInfo),
	}

	s.chanScheduler = batch.NewTimeScheduler(
		db, &s.cacheMu, opts.BatchCommitInterval,
	)
	s.nodeScheduler = batch.NewTimeScheduler(
		db, nil, opts.BatchCommitInterval,
	)

	return s, nil
}

// AddNode adds a vertex/node to the graph database. If the node is not
// in the database from before, this will add a new, unconnected one to the
// graph. If it is present from before, this will update that node's
// information.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) AddNode(ctx context.Context,
	node *models.Node, opts ...batch.SchedulerOption) error {

	r := &batch.Request[SQLQueries]{
		Opts: batch.NewSchedulerOptions(opts...),
		Do: func(queries SQLQueries) error {
			_, err := upsertNode(ctx, queries, node)

			// It is possible that two of the same node
			// announcements are both being processed in the same
			// batch. This may case the UpsertNode conflict to
			// be hit since we require at the db layer that the
			// new last_update is greater than the existing
			// last_update. We need to gracefully handle this here.
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}

			return err
		},
	}

	return s.nodeScheduler.Execute(ctx, r)
}

// FetchNode attempts to look up a target node by its identity public
// key. If the node isn't found in the database, then ErrGraphNodeNotFound is
// returned.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) FetchNode(ctx context.Context,
	pubKey route.Vertex) (*models.Node, error) {

	var node *models.Node
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		var err error
		_, node, err = getNodeByPubKey(ctx, s.cfg.QueryCfg, db, pubKey)

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch node: %w", err)
	}

	return node, nil
}

// HasNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) HasNode(ctx context.Context,
	pubKey [33]byte) (time.Time, bool, error) {

	var (
		exists     bool
		lastUpdate time.Time
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbNode, err := db.GetNodeByPubKey(
			ctx, sqlc.GetNodeByPubKeyParams{
				Version: int16(lnwire.GossipVersion1),
				PubKey:  pubKey[:],
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("unable to fetch node: %w", err)
		}

		exists = true

		if dbNode.LastUpdate.Valid {
			lastUpdate = time.Unix(dbNode.LastUpdate.Int64, 0)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return time.Time{}, false,
			fmt.Errorf("unable to fetch node: %w", err)
	}

	return lastUpdate, exists, nil
}

// AddrsForNode returns all known addresses for the target node public key
// that the graph DB is aware of. The returned boolean indicates if the
// given node is unknown to the graph DB or not.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	var (
		addresses []net.Addr
		known     bool
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// First, check if the node exists and get its DB ID if it
		// does.
		dbID, err := db.GetNodeIDByPubKey(
			ctx, sqlc.GetNodeIDByPubKeyParams{
				Version: int16(lnwire.GossipVersion1),
				PubKey:  nodePub.SerializeCompressed(),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}

		known = true

		addresses, err = getNodeAddresses(ctx, db, dbID)
		if err != nil {
			return fmt.Errorf("unable to fetch node addresses: %w",
				err)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return false, nil, fmt.Errorf("unable to get addresses for "+
			"node(%x): %w", nodePub.SerializeCompressed(), err)
	}

	return known, addresses, nil
}

// DeleteNode starts a new database transaction to remove a vertex/node
// from the database according to the node's public key.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) DeleteNode(ctx context.Context,
	pubKey route.Vertex) error {

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		res, err := db.DeleteNodeByPubKey(
			ctx, sqlc.DeleteNodeByPubKeyParams{
				Version: int16(lnwire.GossipVersion1),
				PubKey:  pubKey[:],
			},
		)
		if err != nil {
			return err
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}

		if rows == 0 {
			return ErrGraphNodeNotFound
		} else if rows > 1 {
			return fmt.Errorf("deleted %d rows, expected 1", rows)
		}

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to delete node: %w", err)
	}

	return nil
}

// FetchNodeFeatures returns the features of the given node. If no features are
// known for the node, an empty feature vector is returned.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (s *SQLStore) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	ctx := context.TODO()

	return fetchNodeFeatures(ctx, s.db, nodePub)
}

// DisabledChannelIDs returns the channel ids of disabled channels.
// A channel is disabled when two of the associated ChanelEdgePolicies
// have their disabled bit on.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) DisabledChannelIDs() ([]uint64, error) {
	var (
		ctx     = context.TODO()
		chanIDs []uint64
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbChanIDs, err := db.GetV1DisabledSCIDs(ctx)
		if err != nil {
			return fmt.Errorf("unable to fetch disabled "+
				"channels: %w", err)
		}

		chanIDs = fn.Map(dbChanIDs, byteOrder.Uint64)

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch disabled channels: %w",
			err)
	}

	return chanIDs, nil
}

// LookupAlias attempts to return the alias as advertised by the target node.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	var alias string
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbNode, err := db.GetNodeByPubKey(
			ctx, sqlc.GetNodeByPubKeyParams{
				Version: int16(lnwire.GossipVersion1),
				PubKey:  pub.SerializeCompressed(),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNodeAliasNotFound
		} else if err != nil {
			return fmt.Errorf("unable to fetch node: %w", err)
		}

		if !dbNode.Alias.Valid {
			return ErrNodeAliasNotFound
		}

		alias = dbNode.Alias.String

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return "", fmt.Errorf("unable to look up alias: %w", err)
	}

	return alias, nil
}

// SourceNode returns the source node of the graph. The source node is treated
// as the center node within a star-graph. This method may be used to kick off
// a path finding algorithm in order to explore the reachability of another
// node based off the source node.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) SourceNode(ctx context.Context) (*models.Node,
	error) {

	var node *models.Node
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		_, nodePub, err := s.getSourceNode(
			ctx, db, lnwire.GossipVersion1,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch V1 source node: %w",
				err)
		}

		_, node, err = getNodeByPubKey(ctx, s.cfg.QueryCfg, db, nodePub)

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch source node: %w", err)
	}

	return node, nil
}

// SetSourceNode sets the source node within the graph database. The source
// node is to be used as the center of a star-graph within path finding
// algorithms.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) SetSourceNode(ctx context.Context,
	node *models.Node) error {

	return s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// For the source node, we use a less strict upsert that allows
		// updates even when the timestamp hasn't changed. This handles
		// the race condition where multiple goroutines (e.g.,
		// setSelfNode, createNewHiddenService, RPC updates) read the
		// same old timestamp, independently increment it, and try to
		// write concurrently. We want all parameter changes to persist,
		// even if timestamps collide.
		id, err := upsertSourceNode(ctx, db, node)
		if err != nil {
			return fmt.Errorf("unable to upsert source node: %w",
				err)
		}

		// Make sure that if a source node for this version is already
		// set, then the ID is the same as the one we are about to set.
		dbSourceNodeID, _, err := s.getSourceNode(
			ctx, db, lnwire.GossipVersion1,
		)
		if err != nil && !errors.Is(err, ErrSourceNodeNotSet) {
			return fmt.Errorf("unable to fetch source node: %w",
				err)
		} else if err == nil {
			if dbSourceNodeID != id {
				return fmt.Errorf("v1 source node already "+
					"set to a different node: %d vs %d",
					dbSourceNodeID, id)
			}

			return nil
		}

		return db.AddSourceNode(ctx, id)
	}, sqldb.NoOpReset)
}

// NodeUpdatesInHorizon returns all the known lightning node which have an
// update timestamp within the passed range. This method can be used by two
// nodes to quickly determine if they have the same set of up to date node
// announcements.
//
// NOTE: This is part of the V1Store interface.
func (s *SQLStore) NodeUpdatesInHorizon(startTime, endTime time.Time,
	opts ...IteratorOption) iter.Seq2[*models.Node, error] {

	cfg := defaultIteratorConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return func(yield func(*models.Node, error) bool) {
		var (
			ctx            = context.TODO()
			lastUpdateTime sql.NullInt64
			lastPubKey     = make([]byte, 33)
			hasMore        = true
		)

		// Each iteration, we'll read a batch amount of nodes, yield
		// them, then decide is we have more or not.
		for hasMore {
			var batch []*models.Node

			//nolint:ll
			err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
				//nolint:ll
				params := sqlc.GetNodesByLastUpdateRangeParams{
					StartTime: sqldb.SQLInt64(
						startTime.Unix(),
					),
					EndTime: sqldb.SQLInt64(
						endTime.Unix(),
					),
					LastUpdate: lastUpdateTime,
					LastPubKey: lastPubKey,
					OnlyPublic: sql.NullBool{
						Bool:  cfg.iterPublicNodes,
						Valid: true,
					},
					MaxResults: sqldb.SQLInt32(
						cfg.nodeUpdateIterBatchSize,
					),
				}
				rows, err := db.GetNodesByLastUpdateRange(
					ctx, params,
				)
				if err != nil {
					return err
				}

				hasMore = len(rows) == cfg.nodeUpdateIterBatchSize

				err = forEachNodeInBatch(
					ctx, s.cfg.QueryCfg, db, rows,
					func(_ int64, node *models.Node) error {
						batch = append(batch, node)

						// Update pagination cursors
						// based on the last processed
						// node.
						lastUpdateTime = sql.NullInt64{
							Int64: node.LastUpdate.
								Unix(),
							Valid: true,
						}
						lastPubKey = node.PubKeyBytes[:]

						return nil
					},
				)
				if err != nil {
					return fmt.Errorf("unable to build "+
						"nodes: %w", err)
				}

				return nil
			}, func() {
				batch = []*models.Node{}
			})

			if err != nil {
				log.Errorf("NodeUpdatesInHorizon batch "+
					"error: %v", err)

				yield(&models.Node{}, err)

				return
			}

			for _, node := range batch {
				if !yield(node, nil) {
					return
				}
			}

			// If the batch didn't yield anything, then we're done.
			if len(batch) == 0 {
				break
			}
		}
	}
}

// AddChannelEdge adds a new (undirected, blank) edge to the graph database. An
// undirected edge from the two target nodes are created. The information stored
// denotes the static attributes of the channel, such as the channelID, the keys
// involved in creation of the channel, and the set of features that the channel
// supports. The chanPoint and chanID are used to uniquely identify the edge
// globally within the database.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) AddChannelEdge(ctx context.Context,
	edge *models.ChannelEdgeInfo, opts ...batch.SchedulerOption) error {

	var alreadyExists bool
	r := &batch.Request[SQLQueries]{
		Opts: batch.NewSchedulerOptions(opts...),
		Reset: func() {
			alreadyExists = false
		},
		Do: func(tx SQLQueries) error {
			chanIDB := channelIDToBytes(edge.ChannelID)

			// Make sure that the channel doesn't already exist. We
			// do this explicitly instead of relying on catching a
			// unique constraint error because relying on SQL to
			// throw that error would abort the entire batch of
			// transactions.
			_, err := tx.GetChannelBySCID(
				ctx, sqlc.GetChannelBySCIDParams{
					Scid:    chanIDB,
					Version: int16(lnwire.GossipVersion1),
				},
			)
			if err == nil {
				alreadyExists = true
				return nil
			} else if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unable to fetch channel: %w",
					err)
			}

			return insertChannel(ctx, tx, edge)
		},
		OnCommit: func(err error) error {
			switch {
			case err != nil:
				return err
			case alreadyExists:
				return ErrEdgeAlreadyExist
			default:
				s.rejectCache.remove(edge.ChannelID)
				s.chanCache.remove(edge.ChannelID)
				return nil
			}
		},
	}

	return s.chanScheduler.Execute(ctx, r)
}

// HighestChanID returns the "highest" known channel ID in the channel graph.
// This represents the "newest" channel from the PoV of the chain. This method
// can be used by peers to quickly determine if their graphs are in sync.
//
// NOTE: This is part of the V1Store interface.
func (s *SQLStore) HighestChanID(ctx context.Context) (uint64, error) {
	var highestChanID uint64
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		chanID, err := db.HighestSCID(ctx, int16(lnwire.GossipVersion1))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("unable to fetch highest chan ID: %w",
				err)
		}

		highestChanID = byteOrder.Uint64(chanID)

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return 0, fmt.Errorf("unable to fetch highest chan ID: %w", err)
	}

	return highestChanID, nil
}

// UpdateEdgePolicy updates the edge routing policy for a single directed edge
// within the database for the referenced channel. The `flags` attribute within
// the ChannelEdgePolicy determines which of the directed edges are being
// updated. If the flag is 1, then the first node's information is being
// updated, otherwise it's the second node's information. The node ordering is
// determined by the lexicographical ordering of the identity public keys of the
// nodes on either side of the channel.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) UpdateEdgePolicy(ctx context.Context,
	edge *models.ChannelEdgePolicy,
	opts ...batch.SchedulerOption) (route.Vertex, route.Vertex, error) {

	var (
		isUpdate1    bool
		edgeNotFound bool
		from, to     route.Vertex
	)

	r := &batch.Request[SQLQueries]{
		Opts: batch.NewSchedulerOptions(opts...),
		Reset: func() {
			isUpdate1 = false
			edgeNotFound = false
		},
		Do: func(tx SQLQueries) error {
			var err error
			from, to, isUpdate1, err = updateChanEdgePolicy(
				ctx, tx, edge,
			)
			// It is possible that two of the same policy
			// announcements are both being processed in the same
			// batch. This may case the UpsertEdgePolicy conflict to
			// be hit since we require at the db layer that the
			// new last_update is greater than the existing
			// last_update. We need to gracefully handle this here.
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			} else if err != nil {
				log.Errorf("UpdateEdgePolicy faild: %v", err)
			}

			// Silence ErrEdgeNotFound so that the batch can
			// succeed, but propagate the error via local state.
			if errors.Is(err, ErrEdgeNotFound) {
				edgeNotFound = true
				return nil
			}

			return err
		},
		OnCommit: func(err error) error {
			switch {
			case err != nil:
				return err
			case edgeNotFound:
				return ErrEdgeNotFound
			default:
				s.updateEdgeCache(edge, isUpdate1)
				return nil
			}
		},
	}

	err := s.chanScheduler.Execute(ctx, r)

	return from, to, err
}

// updateEdgeCache updates our reject and channel caches with the new
// edge policy information.
func (s *SQLStore) updateEdgeCache(e *models.ChannelEdgePolicy,
	isUpdate1 bool) {

	// If an entry for this channel is found in reject cache, we'll modify
	// the entry with the updated timestamp for the direction that was just
	// written. If the edge doesn't exist, we'll load the cache entry lazily
	// during the next query for this edge.
	if entry, ok := s.rejectCache.get(e.ChannelID); ok {
		if isUpdate1 {
			entry.upd1Time = e.LastUpdate.Unix()
		} else {
			entry.upd2Time = e.LastUpdate.Unix()
		}
		s.rejectCache.insert(e.ChannelID, entry)
	}

	// If an entry for this channel is found in channel cache, we'll modify
	// the entry with the updated policy for the direction that was just
	// written. If the edge doesn't exist, we'll defer loading the info and
	// policies and lazily read from disk during the next query.
	if channel, ok := s.chanCache.get(e.ChannelID); ok {
		if isUpdate1 {
			channel.Policy1 = e
		} else {
			channel.Policy2 = e
		}
		s.chanCache.insert(e.ChannelID, channel)
	}
}

// ForEachSourceNodeChannel iterates through all channels of the source node,
// executing the passed callback on each. The call-back is provided with the
// channel's outpoint, whether we have a policy for the channel and the channel
// peer's node information.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ForEachSourceNodeChannel(ctx context.Context,
	cb func(chanPoint wire.OutPoint, havePolicy bool,
		otherNode *models.Node) error, reset func()) error {

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		nodeID, nodePub, err := s.getSourceNode(
			ctx, db, lnwire.GossipVersion1,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch source node: %w",
				err)
		}

		return forEachNodeChannel(
			ctx, db, s.cfg, nodeID,
			func(info *models.ChannelEdgeInfo,
				outPolicy *models.ChannelEdgePolicy,
				_ *models.ChannelEdgePolicy) error {

				// Fetch the other node.
				var (
					otherNodePub [33]byte
					node1        = info.NodeKey1Bytes
					node2        = info.NodeKey2Bytes
				)
				switch {
				case bytes.Equal(node1[:], nodePub[:]):
					otherNodePub = node2
				case bytes.Equal(node2[:], nodePub[:]):
					otherNodePub = node1
				default:
					return fmt.Errorf("node not " +
						"participating in this channel")
				}

				_, otherNode, err := getNodeByPubKey(
					ctx, s.cfg.QueryCfg, db, otherNodePub,
				)
				if err != nil {
					return fmt.Errorf("unable to fetch "+
						"other node(%x): %w",
						otherNodePub, err)
				}

				return cb(
					info.ChannelPoint, outPolicy != nil,
					otherNode,
				)
			},
		)
	}, reset)
}

// ForEachNode iterates through all the stored vertices/nodes in the graph,
// executing the passed callback with each node encountered. If the callback
// returns an error, then the transaction is aborted and the iteration stops
// early.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ForEachNode(ctx context.Context,
	cb func(node *models.Node) error, reset func()) error {

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		return forEachNodePaginated(
			ctx, s.cfg.QueryCfg, db,
			lnwire.GossipVersion1, func(_ context.Context, _ int64,
				node *models.Node) error {

				return cb(node)
			},
		)
	}, reset)
}

// ForEachNodeDirectedChannel iterates through all channels of a given node,
// executing the passed callback on the directed edge representing the channel
// and its incoming policy. If the callback returns an error, then the iteration
// is halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: this is part of the graphdb.NodeTraverser interface.
func (s *SQLStore) ForEachNodeDirectedChannel(nodePub route.Vertex,
	cb func(channel *DirectedChannel) error, reset func()) error {

	var ctx = context.TODO()

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		return forEachNodeDirectedChannel(ctx, db, nodePub, cb)
	}, reset)
}

// ForEachNodeCacheable iterates through all the stored vertices/nodes in the
// graph, executing the passed callback with each node encountered. If the
// callback returns an error, then the transaction is aborted and the iteration
// stops early.
func (s *SQLStore) ForEachNodeCacheable(ctx context.Context,
	cb func(route.Vertex, *lnwire.FeatureVector) error,
	reset func()) error {

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		return forEachNodeCacheable(
			ctx, s.cfg.QueryCfg, db,
			func(_ int64, nodePub route.Vertex,
				features *lnwire.FeatureVector) error {

				return cb(nodePub, features)
			},
		)
	}, reset)
	if err != nil {
		return fmt.Errorf("unable to fetch nodes: %w", err)
	}

	return nil
}

// ForEachNodeChannel iterates through all channels of the given node,
// executing the passed callback with an edge info structure and the policies
// of each end of the channel. The first edge policy is the outgoing edge *to*
// the connecting node, while the second is the incoming edge *from* the
// connecting node. If the callback returns an error, then the iteration is
// halted with the error propagated back up to the caller.
//
// Unknown policies are passed into the callback as nil values.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ForEachNodeChannel(ctx context.Context, nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbNode, err := db.GetNodeByPubKey(
			ctx, sqlc.GetNodeByPubKeyParams{
				Version: int16(lnwire.GossipVersion1),
				PubKey:  nodePub[:],
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("unable to fetch node: %w", err)
		}

		return forEachNodeChannel(ctx, db, s.cfg, dbNode.ID, cb)
	}, reset)
}

// extractMaxUpdateTime returns the maximum of the two policy update times.
// This is used for pagination cursor tracking.
func extractMaxUpdateTime(
	row sqlc.GetChannelsByPolicyLastUpdateRangeRow) int64 {

	switch {
	case row.Policy1LastUpdate.Valid && row.Policy2LastUpdate.Valid:
		return max(row.Policy1LastUpdate.Int64,
			row.Policy2LastUpdate.Int64)
	case row.Policy1LastUpdate.Valid:
		return row.Policy1LastUpdate.Int64
	case row.Policy2LastUpdate.Valid:
		return row.Policy2LastUpdate.Int64
	default:
		return 0
	}
}

// buildChannelFromRow constructs a ChannelEdge from a database row.
// This includes building the nodes, channel info, and policies.
func (s *SQLStore) buildChannelFromRow(ctx context.Context, db SQLQueries,
	row sqlc.GetChannelsByPolicyLastUpdateRangeRow) (ChannelEdge, error) {

	node1, err := buildNode(ctx, s.cfg.QueryCfg, db, row.GraphNode)
	if err != nil {
		return ChannelEdge{}, fmt.Errorf("unable to build node1: %w",
			err)
	}

	node2, err := buildNode(ctx, s.cfg.QueryCfg, db, row.GraphNode_2)
	if err != nil {
		return ChannelEdge{}, fmt.Errorf("unable to build node2: %w",
			err)
	}

	channel, err := getAndBuildEdgeInfo(
		ctx, s.cfg, db,
		row.GraphChannel, node1.PubKeyBytes,
		node2.PubKeyBytes,
	)
	if err != nil {
		return ChannelEdge{}, fmt.Errorf("unable to build "+
			"channel info: %w", err)
	}

	dbPol1, dbPol2, err := extractChannelPolicies(row)
	if err != nil {
		return ChannelEdge{}, fmt.Errorf("unable to extract "+
			"channel policies: %w", err)
	}

	p1, p2, err := getAndBuildChanPolicies(
		ctx, s.cfg.QueryCfg, db, dbPol1, dbPol2, channel.ChannelID,
		node1.PubKeyBytes, node2.PubKeyBytes,
	)
	if err != nil {
		return ChannelEdge{}, fmt.Errorf("unable to build "+
			"channel policies: %w", err)
	}

	return ChannelEdge{
		Info:    channel,
		Policy1: p1,
		Policy2: p2,
		Node1:   node1,
		Node2:   node2,
	}, nil
}

// updateChanCacheBatch updates the channel cache with multiple edges at once.
// This method acquires the cache lock only once for the entire batch.
func (s *SQLStore) updateChanCacheBatch(edgesToCache map[uint64]ChannelEdge) {
	if len(edgesToCache) == 0 {
		return
	}

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	for chanID, edge := range edgesToCache {
		s.chanCache.insert(chanID, edge)
	}
}

// ChanUpdatesInHorizon returns all the known channel edges which have at least
// one edge that has an update timestamp within the specified horizon.
//
// Iterator Lifecycle:
// 1. Initialize state (edgesSeen map, cache tracking, pagination cursors)
// 2. Query batch of channels with policies in time range
// 3. For each channel: check if seen, check cache, or build from DB
// 4. Yield channels to caller
// 5. Update cache after successful batch
// 6. Repeat with updated pagination cursor until no more results
//
// NOTE: This is part of the V1Store interface.
func (s *SQLStore) ChanUpdatesInHorizon(startTime, endTime time.Time,
	opts ...IteratorOption) iter.Seq2[ChannelEdge, error] {

	// Apply options.
	cfg := defaultIteratorConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	return func(yield func(ChannelEdge, error) bool) {
		var (
			ctx            = context.TODO()
			edgesSeen      = make(map[uint64]struct{})
			edgesToCache   = make(map[uint64]ChannelEdge)
			hits           int
			total          int
			lastUpdateTime sql.NullInt64
			lastID         sql.NullInt64
			hasMore        = true
		)

		// Each iteration, we'll read a batch amount of channel updates
		// (consulting the cache along the way), yield them, then loop
		// back to decide if we have any more updates to read out.
		for hasMore {
			var batch []ChannelEdge

			// Acquire read lock before starting transaction to
			// ensure consistent lock ordering (cacheMu -> DB) and
			// prevent deadlock with write operations.
			s.cacheMu.RLock()

			err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(),
				func(db SQLQueries) error {
					//nolint:ll
					params := sqlc.GetChannelsByPolicyLastUpdateRangeParams{
						Version: int16(lnwire.GossipVersion1),
						StartTime: sqldb.SQLInt64(
							startTime.Unix(),
						),
						EndTime: sqldb.SQLInt64(
							endTime.Unix(),
						),
						LastUpdateTime: lastUpdateTime,
						LastID:         lastID,
						MaxResults: sql.NullInt32{
							Int32: int32(
								cfg.chanUpdateIterBatchSize,
							),
							Valid: true,
						},
					}
					//nolint:ll
					rows, err := db.GetChannelsByPolicyLastUpdateRange(
						ctx, params,
					)
					if err != nil {
						return err
					}

					//nolint:ll
					hasMore = len(rows) == cfg.chanUpdateIterBatchSize

					//nolint:ll
					for _, row := range rows {
						lastUpdateTime = sql.NullInt64{
							Int64: extractMaxUpdateTime(row),
							Valid: true,
						}
						lastID = sql.NullInt64{
							Int64: row.GraphChannel.ID,
							Valid: true,
						}

						// Skip if we've already
						// processed this channel.
						chanIDInt := byteOrder.Uint64(
							row.GraphChannel.Scid,
						)
						_, ok := edgesSeen[chanIDInt]
						if ok {
							continue
						}

						// Check cache (we already hold
						// shared read lock).
						channel, ok := s.chanCache.get(
							chanIDInt,
						)
						if ok {
							hits++
							total++
							edgesSeen[chanIDInt] = struct{}{}
							batch = append(batch, channel)

							continue
						}

						chanEdge, err := s.buildChannelFromRow(
							ctx, db, row,
						)
						if err != nil {
							return err
						}

						edgesSeen[chanIDInt] = struct{}{}
						edgesToCache[chanIDInt] = chanEdge

						batch = append(batch, chanEdge)

						total++
					}

					return nil
				}, func() {
					batch = nil
					edgesSeen = make(map[uint64]struct{})
					edgesToCache = make(
						map[uint64]ChannelEdge,
					)
				})

			// Release read lock after transaction completes.
			s.cacheMu.RUnlock()

			if err != nil {
				log.Errorf("ChanUpdatesInHorizon "+
					"batch error: %v", err)

				yield(ChannelEdge{}, err)

				return
			}

			for _, edge := range batch {
				if !yield(edge, nil) {
					return
				}
			}

			// Update cache after successful batch yield, setting
			// the cache lock only once for the entire batch.
			s.updateChanCacheBatch(edgesToCache)
			edgesToCache = make(map[uint64]ChannelEdge)

			// If the batch didn't yield anything, then we're done.
			if len(batch) == 0 {
				break
			}
		}

		if total > 0 {
			log.Debugf("ChanUpdatesInHorizon hit percentage: "+
				"%.2f (%d/%d)",
				float64(hits)*100/float64(total), hits, total)
		} else {
			log.Debugf("ChanUpdatesInHorizon returned no edges "+
				"in horizon (%s, %s)", startTime, endTime)
		}
	}
}

// ForEachNodeCached is similar to forEachNode, but it returns DirectedChannel
// data to the call-back. If withAddrs is true, then the call-back will also be
// provided with the addresses associated with the node. The address retrieval
// result in an additional round-trip to the database, so it should only be used
// if the addresses are actually needed.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ForEachNodeCached(ctx context.Context, withAddrs bool,
	cb func(ctx context.Context, node route.Vertex, addrs []net.Addr,
		chans map[uint64]*DirectedChannel) error, reset func()) error {

	type nodeCachedBatchData struct {
		features      map[int64][]int
		addrs         map[int64][]nodeAddress
		chanBatchData *batchChannelData
		chanMap       map[int64][]sqlc.ListChannelsForNodeIDsRow
	}

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// pageQueryFunc is used to query the next page of nodes.
		pageQueryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.ListNodeIDsAndPubKeysRow, error) {

			return db.ListNodeIDsAndPubKeys(
				ctx, sqlc.ListNodeIDsAndPubKeysParams{
					Version: int16(lnwire.GossipVersion1),
					ID:      lastID,
					Limit:   limit,
				},
			)
		}

		// batchDataFunc is then used to batch load the data required
		// for each page of nodes.
		batchDataFunc := func(ctx context.Context,
			nodeIDs []int64) (*nodeCachedBatchData, error) {

			// Batch load node features.
			nodeFeatures, err := batchLoadNodeFeaturesHelper(
				ctx, s.cfg.QueryCfg, db, nodeIDs,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to batch load "+
					"node features: %w", err)
			}

			// Maybe fetch the node's addresses if requested.
			var nodeAddrs map[int64][]nodeAddress
			if withAddrs {
				nodeAddrs, err = batchLoadNodeAddressesHelper(
					ctx, s.cfg.QueryCfg, db, nodeIDs,
				)
				if err != nil {
					return nil, fmt.Errorf("unable to "+
						"batch load node "+
						"addresses: %w", err)
				}
			}

			// Batch load ALL unique channels for ALL nodes in this
			// page.
			allChannels, err := db.ListChannelsForNodeIDs(
				ctx, sqlc.ListChannelsForNodeIDsParams{
					Version:  int16(lnwire.GossipVersion1),
					Node1Ids: nodeIDs,
					Node2Ids: nodeIDs,
				},
			)
			if err != nil {
				return nil, fmt.Errorf("unable to batch "+
					"fetch channels for nodes: %w", err)
			}

			// Deduplicate channels and collect IDs.
			var (
				allChannelIDs []int64
				allPolicyIDs  []int64
			)
			uniqueChannels := make(
				map[int64]sqlc.ListChannelsForNodeIDsRow,
			)

			for _, channel := range allChannels {
				channelID := channel.GraphChannel.ID

				// Only process each unique channel once.
				_, exists := uniqueChannels[channelID]
				if exists {
					continue
				}

				uniqueChannels[channelID] = channel
				allChannelIDs = append(allChannelIDs, channelID)

				if channel.Policy1ID.Valid {
					allPolicyIDs = append(
						allPolicyIDs,
						channel.Policy1ID.Int64,
					)
				}
				if channel.Policy2ID.Valid {
					allPolicyIDs = append(
						allPolicyIDs,
						channel.Policy2ID.Int64,
					)
				}
			}

			// Batch load channel data for all unique channels.
			channelBatchData, err := batchLoadChannelData(
				ctx, s.cfg.QueryCfg, db, allChannelIDs,
				allPolicyIDs,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to batch "+
					"load channel data: %w", err)
			}

			// Create map of node ID to channels that involve this
			// node.
			nodeIDSet := make(map[int64]bool)
			for _, nodeID := range nodeIDs {
				nodeIDSet[nodeID] = true
			}

			nodeChannelMap := make(
				map[int64][]sqlc.ListChannelsForNodeIDsRow,
			)
			for _, channel := range uniqueChannels {
				// Add channel to both nodes if they're in our
				// current page.
				node1 := channel.GraphChannel.NodeID1
				if nodeIDSet[node1] {
					nodeChannelMap[node1] = append(
						nodeChannelMap[node1], channel,
					)
				}
				node2 := channel.GraphChannel.NodeID2
				if nodeIDSet[node2] {
					nodeChannelMap[node2] = append(
						nodeChannelMap[node2], channel,
					)
				}
			}

			return &nodeCachedBatchData{
				features:      nodeFeatures,
				addrs:         nodeAddrs,
				chanBatchData: channelBatchData,
				chanMap:       nodeChannelMap,
			}, nil
		}

		// processItem is used to process each node in the current page.
		processItem := func(ctx context.Context,
			nodeData sqlc.ListNodeIDsAndPubKeysRow,
			batchData *nodeCachedBatchData) error {

			// Build feature vector for this node.
			fv := lnwire.EmptyFeatureVector()
			features, exists := batchData.features[nodeData.ID]
			if exists {
				for _, bit := range features {
					fv.Set(lnwire.FeatureBit(bit))
				}
			}

			var nodePub route.Vertex
			copy(nodePub[:], nodeData.PubKey)

			nodeChannels := batchData.chanMap[nodeData.ID]

			toNodeCallback := func() route.Vertex {
				return nodePub
			}

			// Build cached channels map for this node.
			channels := make(map[uint64]*DirectedChannel)
			for _, channelRow := range nodeChannels {
				directedChan, err := buildDirectedChannel(
					s.cfg.ChainHash, nodeData.ID, nodePub,
					channelRow, batchData.chanBatchData, fv,
					toNodeCallback,
				)
				if err != nil {
					return err
				}

				channels[directedChan.ChannelID] = directedChan
			}

			addrs, err := buildNodeAddresses(
				batchData.addrs[nodeData.ID],
			)
			if err != nil {
				return fmt.Errorf("unable to build node "+
					"addresses: %w", err)
			}

			return cb(ctx, nodePub, addrs, channels)
		}

		return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
			ctx, s.cfg.QueryCfg, int64(-1), pageQueryFunc,
			func(node sqlc.ListNodeIDsAndPubKeysRow) int64 {
				return node.ID
			},
			func(node sqlc.ListNodeIDsAndPubKeysRow) (int64,
				error) {

				return node.ID, nil
			},
			batchDataFunc, processItem,
		)
	}, reset)
}

// ForEachChannelCacheable iterates through all the channel edges stored
// within the graph and invokes the passed callback for each edge. The
// callback takes two edges as since this is a directed graph, both the
// in/out edges are visited. If the callback returns an error, then the
// transaction is aborted and the iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil
// pointer for that particular channel edge routing policy will be
// passed into the callback.
//
// NOTE: this method is like ForEachChannel but fetches only the data
// required for the graph cache.
func (s *SQLStore) ForEachChannelCacheable(cb func(*models.CachedEdgeInfo,
	*models.CachedEdgePolicy, *models.CachedEdgePolicy) error,
	reset func()) error {

	ctx := context.TODO()

	handleChannel := func(_ context.Context,
		row sqlc.ListChannelsWithPoliciesForCachePaginatedRow) error {

		node1, node2, err := buildNodeVertices(
			row.Node1Pubkey, row.Node2Pubkey,
		)
		if err != nil {
			return err
		}

		edge := buildCacheableChannelInfo(
			row.Scid, row.Capacity.Int64, node1, node2,
		)

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return err
		}

		pol1, pol2, err := buildCachedChanPolicies(
			dbPol1, dbPol2, edge.ChannelID, node1, node2,
		)
		if err != nil {
			return err
		}

		return cb(edge, pol1, pol2)
	}

	extractCursor := func(
		row sqlc.ListChannelsWithPoliciesForCachePaginatedRow) int64 {

		return row.ID
	}

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		//nolint:ll
		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.ListChannelsWithPoliciesForCachePaginatedRow,
			error) {

			return db.ListChannelsWithPoliciesForCachePaginated(
				ctx, sqlc.ListChannelsWithPoliciesForCachePaginatedParams{
					Version: int16(lnwire.GossipVersion1),
					ID:      lastID,
					Limit:   limit,
				},
			)
		}

		return sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, int64(-1), queryFunc,
			extractCursor, handleChannel,
		)
	}, reset)
}

// ForEachChannel iterates through all the channel edges stored within the
// graph and invokes the passed callback for each edge. The callback takes two
// edges as since this is a directed graph, both the in/out edges are visited.
// If the callback returns an error, then the transaction is aborted and the
// iteration stops early.
//
// NOTE: If an edge can't be found, or wasn't advertised, then a nil pointer
// for that particular channel edge routing policy will be passed into the
// callback.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error, reset func()) error {

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		return forEachChannelWithPolicies(ctx, db, s.cfg, cb)
	}, reset)
}

// FilterChannelRange returns the channel ID's of all known channels which were
// mined in a block height within the passed range. The channel IDs are grouped
// by their common block height. This method can be used to quickly share with a
// peer the set of channels we know of within a particular range to catch them
// up after a period of time offline. If withTimestamps is true then the
// timestamp info of the latest received channel update messages of the channel
// will be included in the response.
//
// NOTE: This is part of the V1Store interface.
func (s *SQLStore) FilterChannelRange(startHeight, endHeight uint32,
	withTimestamps bool) ([]BlockChannelRange, error) {

	var (
		ctx       = context.TODO()
		startSCID = &lnwire.ShortChannelID{
			BlockHeight: startHeight,
		}
		endSCID = lnwire.ShortChannelID{
			BlockHeight: endHeight,
			TxIndex:     math.MaxUint32 & 0x00ffffff,
			TxPosition:  math.MaxUint16,
		}
		chanIDStart = channelIDToBytes(startSCID.ToUint64())
		chanIDEnd   = channelIDToBytes(endSCID.ToUint64())
	)

	// 1) get all channels where channelID is between start and end chan ID.
	// 2) skip if not public (ie, no channel_proof)
	// 3) collect that channel.
	// 4) if timestamps are wanted, fetch both policies for node 1 and node2
	//    and add those timestamps to the collected channel.
	channelsPerBlock := make(map[uint32][]ChannelUpdateInfo)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		dbChans, err := db.GetPublicV1ChannelsBySCID(
			ctx, sqlc.GetPublicV1ChannelsBySCIDParams{
				StartScid: chanIDStart,
				EndScid:   chanIDEnd,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to fetch channel range: %w",
				err)
		}

		for _, dbChan := range dbChans {
			cid := lnwire.NewShortChanIDFromInt(
				byteOrder.Uint64(dbChan.Scid),
			)
			chanInfo := NewChannelUpdateInfo(
				cid, time.Time{}, time.Time{},
			)

			if !withTimestamps {
				channelsPerBlock[cid.BlockHeight] = append(
					channelsPerBlock[cid.BlockHeight],
					chanInfo,
				)

				continue
			}

			//nolint:ll
			node1Policy, err := db.GetChannelPolicyByChannelAndNode(
				ctx, sqlc.GetChannelPolicyByChannelAndNodeParams{
					Version:   int16(lnwire.GossipVersion1),
					ChannelID: dbChan.ID,
					NodeID:    dbChan.NodeID1,
				},
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unable to fetch node1 "+
					"policy: %w", err)
			} else if err == nil {
				chanInfo.Node1UpdateTimestamp = time.Unix(
					node1Policy.LastUpdate.Int64, 0,
				)
			}

			//nolint:ll
			node2Policy, err := db.GetChannelPolicyByChannelAndNode(
				ctx, sqlc.GetChannelPolicyByChannelAndNodeParams{
					Version:   int16(lnwire.GossipVersion1),
					ChannelID: dbChan.ID,
					NodeID:    dbChan.NodeID2,
				},
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("unable to fetch node2 "+
					"policy: %w", err)
			} else if err == nil {
				chanInfo.Node2UpdateTimestamp = time.Unix(
					node2Policy.LastUpdate.Int64, 0,
				)
			}

			channelsPerBlock[cid.BlockHeight] = append(
				channelsPerBlock[cid.BlockHeight], chanInfo,
			)
		}

		return nil
	}, func() {
		channelsPerBlock = make(map[uint32][]ChannelUpdateInfo)
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channel range: %w", err)
	}

	if len(channelsPerBlock) == 0 {
		return nil, nil
	}

	// Return the channel ranges in ascending block height order.
	blocks := slices.Collect(maps.Keys(channelsPerBlock))
	slices.Sort(blocks)

	return fn.Map(blocks, func(block uint32) BlockChannelRange {
		return BlockChannelRange{
			Height:   block,
			Channels: channelsPerBlock[block],
		}
	}), nil
}

// MarkEdgeZombie attempts to mark a channel identified by its channel ID as a
// zombie. This method is used on an ad-hoc basis, when channels need to be
// marked as zombies outside the normal pruning cycle.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) MarkEdgeZombie(chanID uint64,
	pubKey1, pubKey2 [33]byte) error {

	ctx := context.TODO()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	chanIDB := channelIDToBytes(chanID)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		return db.UpsertZombieChannel(
			ctx, sqlc.UpsertZombieChannelParams{
				Version:  int16(lnwire.GossipVersion1),
				Scid:     chanIDB,
				NodeKey1: pubKey1[:],
				NodeKey2: pubKey2[:],
			},
		)
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to upsert zombie channel "+
			"(channel_id=%d): %w", chanID, err)
	}

	s.rejectCache.remove(chanID)
	s.chanCache.remove(chanID)

	return nil
}

// MarkEdgeLive clears an edge from our zombie index, deeming it as live.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) MarkEdgeLive(chanID uint64) error {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	var (
		ctx     = context.TODO()
		chanIDB = channelIDToBytes(chanID)
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		res, err := db.DeleteZombieChannel(
			ctx, sqlc.DeleteZombieChannelParams{
				Scid:    chanIDB,
				Version: int16(lnwire.GossipVersion1),
			},
		)
		if err != nil {
			return fmt.Errorf("unable to delete zombie channel: %w",
				err)
		}

		rows, err := res.RowsAffected()
		if err != nil {
			return err
		}

		if rows == 0 {
			return ErrZombieEdgeNotFound
		} else if rows > 1 {
			return fmt.Errorf("deleted %d zombie rows, "+
				"expected 1", rows)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to mark edge live "+
			"(channel_id=%d): %w", chanID, err)
	}

	s.rejectCache.remove(chanID)
	s.chanCache.remove(chanID)

	return err
}

// IsZombieEdge returns whether the edge is considered zombie. If it is a
// zombie, then the two node public keys corresponding to this edge are also
// returned.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) IsZombieEdge(chanID uint64) (bool, [33]byte, [33]byte,
	error) {

	var (
		ctx              = context.TODO()
		isZombie         bool
		pubKey1, pubKey2 route.Vertex
		chanIDB          = channelIDToBytes(chanID)
	)

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		zombie, err := db.GetZombieChannel(
			ctx, sqlc.GetZombieChannelParams{
				Scid:    chanIDB,
				Version: int16(lnwire.GossipVersion1),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("unable to fetch zombie channel: %w",
				err)
		}

		copy(pubKey1[:], zombie.NodeKey1)
		copy(pubKey2[:], zombie.NodeKey2)
		isZombie = true

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return false, route.Vertex{}, route.Vertex{},
			fmt.Errorf("%w: %w (chanID=%d)",
				ErrCantCheckIfZombieEdgeStr, err, chanID)
	}

	return isZombie, pubKey1, pubKey2, nil
}

// NumZombies returns the current number of zombie channels in the graph.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) NumZombies() (uint64, error) {
	var (
		ctx        = context.TODO()
		numZombies uint64
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		count, err := db.CountZombieChannels(
			ctx, int16(lnwire.GossipVersion1),
		)
		if err != nil {
			return fmt.Errorf("unable to count zombie channels: %w",
				err)
		}

		numZombies = uint64(count)

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return 0, fmt.Errorf("unable to count zombies: %w", err)
	}

	return numZombies, nil
}

// DeleteChannelEdges removes edges with the given channel IDs from the
// database and marks them as zombies. This ensures that we're unable to re-add
// it to our database once again. If an edge does not exist within the
// database, then ErrEdgeNotFound will be returned. If strictZombiePruning is
// true, then when we mark these edges as zombies, we'll set up the keys such
// that we require the node that failed to send the fresh update to be the one
// that resurrects the channel from its zombie state. The markZombie bool
// denotes whether to mark the channel as a zombie.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) DeleteChannelEdges(strictZombiePruning, markZombie bool,
	chanIDs ...uint64) ([]*models.ChannelEdgeInfo, error) {

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	// Keep track of which channels we end up finding so that we can
	// correctly return ErrEdgeNotFound if we do not find a channel.
	chanLookup := make(map[uint64]struct{}, len(chanIDs))
	for _, chanID := range chanIDs {
		chanLookup[chanID] = struct{}{}
	}

	var (
		ctx   = context.TODO()
		edges []*models.ChannelEdgeInfo
	)
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// First, collect all channel rows.
		var channelRows []sqlc.GetChannelsBySCIDWithPoliciesRow
		chanCallBack := func(ctx context.Context,
			row sqlc.GetChannelsBySCIDWithPoliciesRow) error {

			// Deleting the entry from the map indicates that we
			// have found the channel.
			scid := byteOrder.Uint64(row.GraphChannel.Scid)
			delete(chanLookup, scid)

			channelRows = append(channelRows, row)

			return nil
		}

		err := s.forEachChanWithPoliciesInSCIDList(
			ctx, db, chanCallBack, chanIDs,
		)
		if err != nil {
			return err
		}

		if len(chanLookup) > 0 {
			return ErrEdgeNotFound
		}

		if len(channelRows) == 0 {
			return nil
		}

		// Batch build all channel edges.
		var chanIDsToDelete []int64
		edges, chanIDsToDelete, err = batchBuildChannelInfo(
			ctx, s.cfg, db, channelRows,
		)
		if err != nil {
			return err
		}

		if markZombie {
			for i, row := range channelRows {
				scid := byteOrder.Uint64(row.GraphChannel.Scid)

				err := handleZombieMarking(
					ctx, db, row, edges[i],
					strictZombiePruning, scid,
				)
				if err != nil {
					return fmt.Errorf("unable to mark "+
						"channel as zombie: %w", err)
				}
			}
		}

		return s.deleteChannels(ctx, db, chanIDsToDelete)
	}, func() {
		edges = nil

		// Re-fill the lookup map.
		for _, chanID := range chanIDs {
			chanLookup[chanID] = struct{}{}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("unable to delete channel edges: %w",
			err)
	}

	for _, chanID := range chanIDs {
		s.rejectCache.remove(chanID)
		s.chanCache.remove(chanID)
	}

	return edges, nil
}

// FetchChannelEdgesByID attempts to lookup the two directed edges for the
// channel identified by the channel ID. If the channel can't be found, then
// ErrEdgeNotFound is returned. A struct which houses the general information
// for the channel itself is returned as well as two structs that contain the
// routing policies for the channel in either direction.
//
// ErrZombieEdge an be returned if the edge is currently marked as a zombie
// within the database. In this case, the ChannelEdgePolicy's will be nil, and
// the ChannelEdgeInfo will only include the public keys of each node.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) FetchChannelEdgesByID(chanID uint64) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	var (
		ctx              = context.TODO()
		edge             *models.ChannelEdgeInfo
		policy1, policy2 *models.ChannelEdgePolicy
		chanIDB          = channelIDToBytes(chanID)
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		row, err := db.GetChannelBySCIDWithPolicies(
			ctx, sqlc.GetChannelBySCIDWithPoliciesParams{
				Scid:    chanIDB,
				Version: int16(lnwire.GossipVersion1),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			// First check if this edge is perhaps in the zombie
			// index.
			zombie, err := db.GetZombieChannel(
				ctx, sqlc.GetZombieChannelParams{
					Scid:    chanIDB,
					Version: int16(lnwire.GossipVersion1),
				},
			)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEdgeNotFound
			} else if err != nil {
				return fmt.Errorf("unable to check if "+
					"channel is zombie: %w", err)
			}

			// At this point, we know the channel is a zombie, so
			// we'll return an error indicating this, and we will
			// populate the edge info with the public keys of each
			// party as this is the only information we have about
			// it.
			edge = &models.ChannelEdgeInfo{}
			copy(edge.NodeKey1Bytes[:], zombie.NodeKey1)
			copy(edge.NodeKey2Bytes[:], zombie.NodeKey2)

			return ErrZombieEdge
		} else if err != nil {
			return fmt.Errorf("unable to fetch channel: %w", err)
		}

		node1, node2, err := buildNodeVertices(
			row.GraphNode.PubKey, row.GraphNode_2.PubKey,
		)
		if err != nil {
			return err
		}

		edge, err = getAndBuildEdgeInfo(
			ctx, s.cfg, db, row.GraphChannel, node1, node2,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel info: %w",
				err)
		}

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return fmt.Errorf("unable to extract channel "+
				"policies: %w", err)
		}

		policy1, policy2, err = getAndBuildChanPolicies(
			ctx, s.cfg.QueryCfg, db, dbPol1, dbPol2, edge.ChannelID,
			node1, node2,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel "+
				"policies: %w", err)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		// If we are returning the ErrZombieEdge, then we also need to
		// return the edge info as the method comment indicates that
		// this will be populated when the edge is a zombie.
		return edge, nil, nil, fmt.Errorf("could not fetch channel: %w",
			err)
	}

	return edge, policy1, policy2, nil
}

// FetchChannelEdgesByOutpoint attempts to lookup the two directed edges for
// the channel identified by the funding outpoint. If the channel can't be
// found, then ErrEdgeNotFound is returned. A struct which houses the general
// information for the channel itself is returned as well as two structs that
// contain the routing policies for the channel in either direction.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) FetchChannelEdgesByOutpoint(op *wire.OutPoint) (
	*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	var (
		ctx              = context.TODO()
		edge             *models.ChannelEdgeInfo
		policy1, policy2 *models.ChannelEdgePolicy
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		row, err := db.GetChannelByOutpointWithPolicies(
			ctx, sqlc.GetChannelByOutpointWithPoliciesParams{
				Outpoint: op.String(),
				Version:  int16(lnwire.GossipVersion1),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEdgeNotFound
		} else if err != nil {
			return fmt.Errorf("unable to fetch channel: %w", err)
		}

		node1, node2, err := buildNodeVertices(
			row.Node1Pubkey, row.Node2Pubkey,
		)
		if err != nil {
			return err
		}

		edge, err = getAndBuildEdgeInfo(
			ctx, s.cfg, db, row.GraphChannel, node1, node2,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel info: %w",
				err)
		}

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return fmt.Errorf("unable to extract channel "+
				"policies: %w", err)
		}

		policy1, policy2, err = getAndBuildChanPolicies(
			ctx, s.cfg.QueryCfg, db, dbPol1, dbPol2, edge.ChannelID,
			node1, node2,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel "+
				"policies: %w", err)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("could not fetch channel: %w",
			err)
	}

	return edge, policy1, policy2, nil
}

// HasChannelEdge returns true if the database knows of a channel edge with the
// passed channel ID, and false otherwise. If an edge with that ID is found
// within the graph, then two time stamps representing the last time the edge
// was updated for both directed edges are returned along with the boolean. If
// it is not found, then the zombie index is checked and its result is returned
// as the second boolean.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) HasChannelEdge(chanID uint64) (time.Time, time.Time, bool,
	bool, error) {

	ctx := context.TODO()

	var (
		exists          bool
		isZombie        bool
		node1LastUpdate time.Time
		node2LastUpdate time.Time
	)

	// We'll query the cache with the shared lock held to allow multiple
	// readers to access values in the cache concurrently if they exist.
	s.cacheMu.RLock()
	if entry, ok := s.rejectCache.get(chanID); ok {
		s.cacheMu.RUnlock()
		node1LastUpdate = time.Unix(entry.upd1Time, 0)
		node2LastUpdate = time.Unix(entry.upd2Time, 0)
		exists, isZombie = entry.flags.unpack()

		return node1LastUpdate, node2LastUpdate, exists, isZombie, nil
	}
	s.cacheMu.RUnlock()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	// The item was not found with the shared lock, so we'll acquire the
	// exclusive lock and check the cache again in case another method added
	// the entry to the cache while no lock was held.
	if entry, ok := s.rejectCache.get(chanID); ok {
		node1LastUpdate = time.Unix(entry.upd1Time, 0)
		node2LastUpdate = time.Unix(entry.upd2Time, 0)
		exists, isZombie = entry.flags.unpack()

		return node1LastUpdate, node2LastUpdate, exists, isZombie, nil
	}

	chanIDB := channelIDToBytes(chanID)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		channel, err := db.GetChannelBySCID(
			ctx, sqlc.GetChannelBySCIDParams{
				Scid:    chanIDB,
				Version: int16(lnwire.GossipVersion1),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			// Check if it is a zombie channel.
			isZombie, err = db.IsZombieChannel(
				ctx, sqlc.IsZombieChannelParams{
					Scid:    chanIDB,
					Version: int16(lnwire.GossipVersion1),
				},
			)
			if err != nil {
				return fmt.Errorf("could not check if channel "+
					"is zombie: %w", err)
			}

			return nil
		} else if err != nil {
			return fmt.Errorf("unable to fetch channel: %w", err)
		}

		exists = true

		policy1, err := db.GetChannelPolicyByChannelAndNode(
			ctx, sqlc.GetChannelPolicyByChannelAndNodeParams{
				Version:   int16(lnwire.GossipVersion1),
				ChannelID: channel.ID,
				NodeID:    channel.NodeID1,
			},
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unable to fetch channel policy: %w",
				err)
		} else if err == nil {
			node1LastUpdate = time.Unix(policy1.LastUpdate.Int64, 0)
		}

		policy2, err := db.GetChannelPolicyByChannelAndNode(
			ctx, sqlc.GetChannelPolicyByChannelAndNodeParams{
				Version:   int16(lnwire.GossipVersion1),
				ChannelID: channel.ID,
				NodeID:    channel.NodeID2,
			},
		)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("unable to fetch channel policy: %w",
				err)
		} else if err == nil {
			node2LastUpdate = time.Unix(policy2.LastUpdate.Int64, 0)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return time.Time{}, time.Time{}, false, false,
			fmt.Errorf("unable to fetch channel: %w", err)
	}

	s.rejectCache.insert(chanID, rejectCacheEntry{
		upd1Time: node1LastUpdate.Unix(),
		upd2Time: node2LastUpdate.Unix(),
		flags:    packRejectFlags(exists, isZombie),
	})

	return node1LastUpdate, node2LastUpdate, exists, isZombie, nil
}

// ChannelID attempt to lookup the 8-byte compact channel ID which maps to the
// passed channel point (outpoint). If the passed channel doesn't exist within
// the database, then ErrEdgeNotFound is returned.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ChannelID(chanPoint *wire.OutPoint) (uint64, error) {
	var (
		ctx       = context.TODO()
		channelID uint64
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		chanID, err := db.GetSCIDByOutpoint(
			ctx, sqlc.GetSCIDByOutpointParams{
				Outpoint: chanPoint.String(),
				Version:  int16(lnwire.GossipVersion1),
			},
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEdgeNotFound
		} else if err != nil {
			return fmt.Errorf("unable to fetch channel ID: %w",
				err)
		}

		channelID = byteOrder.Uint64(chanID)

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return 0, fmt.Errorf("unable to fetch channel ID: %w", err)
	}

	return channelID, nil
}

// IsPublicNode is a helper method that determines whether the node with the
// given public key is seen as a public node in the graph from the graph's
// source node's point of view.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) IsPublicNode(pubKey [33]byte) (bool, error) {
	ctx := context.TODO()

	var isPublic bool
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		var err error
		isPublic, err = db.IsPublicV1Node(ctx, pubKey[:])

		return err
	}, sqldb.NoOpReset)
	if err != nil {
		return false, fmt.Errorf("unable to check if node is "+
			"public: %w", err)
	}

	return isPublic, nil
}

// FetchChanInfos returns the set of channel edges that correspond to the passed
// channel ID's. If an edge is the query is unknown to the database, it will
// skipped and the result will contain only those edges that exist at the time
// of the query. This can be used to respond to peer queries that are seeking to
// fill in gaps in their view of the channel graph.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) FetchChanInfos(chanIDs []uint64) ([]ChannelEdge, error) {
	var (
		ctx   = context.TODO()
		edges = make(map[uint64]ChannelEdge)
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// First, collect all channel rows.
		var channelRows []sqlc.GetChannelsBySCIDWithPoliciesRow
		chanCallBack := func(ctx context.Context,
			row sqlc.GetChannelsBySCIDWithPoliciesRow) error {

			channelRows = append(channelRows, row)
			return nil
		}

		err := s.forEachChanWithPoliciesInSCIDList(
			ctx, db, chanCallBack, chanIDs,
		)
		if err != nil {
			return err
		}

		if len(channelRows) == 0 {
			return nil
		}

		// Batch build all channel edges.
		chans, err := batchBuildChannelEdges(
			ctx, s.cfg, db, channelRows,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel edges: %w",
				err)
		}

		for _, c := range chans {
			edges[c.Info.ChannelID] = c
		}

		return err
	}, func() {
		clear(edges)
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels: %w", err)
	}

	res := make([]ChannelEdge, 0, len(edges))
	for _, chanID := range chanIDs {
		edge, ok := edges[chanID]
		if !ok {
			continue
		}

		res = append(res, edge)
	}

	return res, nil
}

// forEachChanWithPoliciesInSCIDList is a wrapper around the
// GetChannelsBySCIDWithPolicies query that allows us to iterate through
// channels in a paginated manner.
func (s *SQLStore) forEachChanWithPoliciesInSCIDList(ctx context.Context,
	db SQLQueries, cb func(ctx context.Context,
		row sqlc.GetChannelsBySCIDWithPoliciesRow) error,
	chanIDs []uint64) error {

	queryWrapper := func(ctx context.Context,
		scids [][]byte) ([]sqlc.GetChannelsBySCIDWithPoliciesRow,
		error) {

		return db.GetChannelsBySCIDWithPolicies(
			ctx, sqlc.GetChannelsBySCIDWithPoliciesParams{
				Version: int16(lnwire.GossipVersion1),
				Scids:   scids,
			},
		)
	}

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, chanIDs, channelIDToBytes, queryWrapper,
		cb,
	)
}

// FilterKnownChanIDs takes a set of channel IDs and return the subset of chan
// ID's that we don't know and are not known zombies of the passed set. In other
// words, we perform a set difference of our set of chan ID's and the ones
// passed in. This method can be used by callers to determine the set of
// channels another peer knows of that we don't. The ChannelUpdateInfos for the
// known zombies is also returned.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) FilterKnownChanIDs(chansInfo []ChannelUpdateInfo) ([]uint64,
	[]ChannelUpdateInfo, error) {

	var (
		ctx          = context.TODO()
		newChanIDs   []uint64
		knownZombies []ChannelUpdateInfo
		infoLookup   = make(
			map[uint64]ChannelUpdateInfo, len(chansInfo),
		)
	)

	// We first build a lookup map of the channel ID's to the
	// ChannelUpdateInfo. This allows us to quickly delete channels that we
	// already know about.
	for _, chanInfo := range chansInfo {
		infoLookup[chanInfo.ShortChannelID.ToUint64()] = chanInfo
	}

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		// The call-back function deletes known channels from
		// infoLookup, so that we can later check which channels are
		// zombies by only looking at the remaining channels in the set.
		cb := func(ctx context.Context,
			channel sqlc.GraphChannel) error {

			delete(infoLookup, byteOrder.Uint64(channel.Scid))

			return nil
		}

		err := s.forEachChanInSCIDList(ctx, db, cb, chansInfo)
		if err != nil {
			return fmt.Errorf("unable to iterate through "+
				"channels: %w", err)
		}

		// We want to ensure that we deal with the channels in the
		// same order that they were passed in, so we iterate over the
		// original chansInfo slice and then check if that channel is
		// still in the infoLookup map.
		for _, chanInfo := range chansInfo {
			channelID := chanInfo.ShortChannelID.ToUint64()
			if _, ok := infoLookup[channelID]; !ok {
				continue
			}

			isZombie, err := db.IsZombieChannel(
				ctx, sqlc.IsZombieChannelParams{
					Scid:    channelIDToBytes(channelID),
					Version: int16(lnwire.GossipVersion1),
				},
			)
			if err != nil {
				return fmt.Errorf("unable to fetch zombie "+
					"channel: %w", err)
			}

			if isZombie {
				knownZombies = append(knownZombies, chanInfo)

				continue
			}

			newChanIDs = append(newChanIDs, channelID)
		}

		return nil
	}, func() {
		newChanIDs = nil
		knownZombies = nil
		// Rebuild the infoLookup map in case of a rollback.
		for _, chanInfo := range chansInfo {
			scid := chanInfo.ShortChannelID.ToUint64()
			infoLookup[scid] = chanInfo
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to fetch channels: %w", err)
	}

	return newChanIDs, knownZombies, nil
}

// forEachChanInSCIDList is a helper method that executes a paged query
// against the database to fetch all channels that match the passed
// ChannelUpdateInfo slice. The callback function is called for each channel
// that is found.
func (s *SQLStore) forEachChanInSCIDList(ctx context.Context, db SQLQueries,
	cb func(ctx context.Context, channel sqlc.GraphChannel) error,
	chansInfo []ChannelUpdateInfo) error {

	queryWrapper := func(ctx context.Context,
		scids [][]byte) ([]sqlc.GraphChannel, error) {

		return db.GetChannelsBySCIDs(
			ctx, sqlc.GetChannelsBySCIDsParams{
				Version: int16(lnwire.GossipVersion1),
				Scids:   scids,
			},
		)
	}

	chanIDConverter := func(chanInfo ChannelUpdateInfo) []byte {
		channelID := chanInfo.ShortChannelID.ToUint64()

		return channelIDToBytes(channelID)
	}

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, chansInfo, chanIDConverter, queryWrapper,
		cb,
	)
}

// PruneGraphNodes is a garbage collection method which attempts to prune out
// any nodes from the channel graph that are currently unconnected. This ensure
// that we only maintain a graph of reachable nodes. In the event that a pruned
// node gains more channels, it will be re-added back to the graph.
//
// NOTE: this prunes nodes across protocol versions. It will never prune the
// source nodes.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) PruneGraphNodes() ([]route.Vertex, error) {
	var ctx = context.TODO()

	var prunedNodes []route.Vertex
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		var err error
		prunedNodes, err = s.pruneGraphNodes(ctx, db)

		return err
	}, func() {
		prunedNodes = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to prune nodes: %w", err)
	}

	return prunedNodes, nil
}

// PruneGraph prunes newly closed channels from the channel graph in response
// to a new block being solved on the network. Any transactions which spend the
// funding output of any known channels within he graph will be deleted.
// Additionally, the "prune tip", or the last block which has been used to
// prune the graph is stored so callers can ensure the graph is fully in sync
// with the current UTXO state. A slice of channels that have been closed by
// the target block along with any pruned nodes are returned if the function
// succeeds without error.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) PruneGraph(spentOutputs []*wire.OutPoint,
	blockHash *chainhash.Hash, blockHeight uint32) (
	[]*models.ChannelEdgeInfo, []route.Vertex, error) {

	ctx := context.TODO()

	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	var (
		closedChans []*models.ChannelEdgeInfo
		prunedNodes []route.Vertex
	)
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		// First, collect all channel rows that need to be pruned.
		var channelRows []sqlc.GetChannelsByOutpointsRow
		channelCallback := func(ctx context.Context,
			row sqlc.GetChannelsByOutpointsRow) error {

			channelRows = append(channelRows, row)

			return nil
		}

		err := s.forEachChanInOutpoints(
			ctx, db, spentOutputs, channelCallback,
		)
		if err != nil {
			return fmt.Errorf("unable to fetch channels by "+
				"outpoints: %w", err)
		}

		if len(channelRows) == 0 {
			// There are no channels to prune. So we can exit early
			// after updating the prune log.
			err = db.UpsertPruneLogEntry(
				ctx, sqlc.UpsertPruneLogEntryParams{
					BlockHash:   blockHash[:],
					BlockHeight: int64(blockHeight),
				},
			)
			if err != nil {
				return fmt.Errorf("unable to insert prune log "+
					"entry: %w", err)
			}

			return nil
		}

		// Batch build all channel edges for pruning.
		var chansToDelete []int64
		closedChans, chansToDelete, err = batchBuildChannelInfo(
			ctx, s.cfg, db, channelRows,
		)
		if err != nil {
			return err
		}

		err = s.deleteChannels(ctx, db, chansToDelete)
		if err != nil {
			return fmt.Errorf("unable to delete channels: %w", err)
		}

		err = db.UpsertPruneLogEntry(
			ctx, sqlc.UpsertPruneLogEntryParams{
				BlockHash:   blockHash[:],
				BlockHeight: int64(blockHeight),
			},
		)
		if err != nil {
			return fmt.Errorf("unable to insert prune log "+
				"entry: %w", err)
		}

		// Now that we've pruned some channels, we'll also prune any
		// nodes that no longer have any channels.
		prunedNodes, err = s.pruneGraphNodes(ctx, db)
		if err != nil {
			return fmt.Errorf("unable to prune graph nodes: %w",
				err)
		}

		return nil
	}, func() {
		prunedNodes = nil
		closedChans = nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to prune graph: %w", err)
	}

	for _, channel := range closedChans {
		s.rejectCache.remove(channel.ChannelID)
		s.chanCache.remove(channel.ChannelID)
	}

	return closedChans, prunedNodes, nil
}

// forEachChanInOutpoints is a helper function that executes a paginated
// query to fetch channels by their outpoints and applies the given call-back
// to each.
//
// NOTE: this fetches channels for all protocol versions.
func (s *SQLStore) forEachChanInOutpoints(ctx context.Context, db SQLQueries,
	outpoints []*wire.OutPoint, cb func(ctx context.Context,
		row sqlc.GetChannelsByOutpointsRow) error) error {

	// Create a wrapper that uses the transaction's db instance to execute
	// the query.
	queryWrapper := func(ctx context.Context,
		pageOutpoints []string) ([]sqlc.GetChannelsByOutpointsRow,
		error) {

		return db.GetChannelsByOutpoints(ctx, pageOutpoints)
	}

	// Define the conversion function from Outpoint to string.
	outpointToString := func(outpoint *wire.OutPoint) string {
		return outpoint.String()
	}

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, outpoints, outpointToString,
		queryWrapper, cb,
	)
}

func (s *SQLStore) deleteChannels(ctx context.Context, db SQLQueries,
	dbIDs []int64) error {

	// Create a wrapper that uses the transaction's db instance to execute
	// the query.
	queryWrapper := func(ctx context.Context, ids []int64) ([]any, error) {
		return nil, db.DeleteChannels(ctx, ids)
	}

	idConverter := func(id int64) int64 {
		return id
	}

	return sqldb.ExecuteBatchQuery(
		ctx, s.cfg.QueryCfg, dbIDs, idConverter,
		queryWrapper, func(ctx context.Context, _ any) error {
			return nil
		},
	)
}

// ChannelView returns the verifiable edge information for each active channel
// within the known channel graph. The set of UTXOs (along with their scripts)
// returned are the ones that need to be watched on chain to detect channel
// closes on the resident blockchain.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) ChannelView() ([]EdgePoint, error) {
	var (
		ctx        = context.TODO()
		edgePoints []EdgePoint
	)

	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		handleChannel := func(_ context.Context,
			channel sqlc.ListChannelsPaginatedRow) error {

			pkScript, err := genMultiSigP2WSH(
				channel.BitcoinKey1, channel.BitcoinKey2,
			)
			if err != nil {
				return err
			}

			op, err := wire.NewOutPointFromString(channel.Outpoint)
			if err != nil {
				return err
			}

			edgePoints = append(edgePoints, EdgePoint{
				FundingPkScript: pkScript,
				OutPoint:        *op,
			})

			return nil
		}

		queryFunc := func(ctx context.Context, lastID int64,
			limit int32) ([]sqlc.ListChannelsPaginatedRow, error) {

			return db.ListChannelsPaginated(
				ctx, sqlc.ListChannelsPaginatedParams{
					Version: int16(lnwire.GossipVersion1),
					ID:      lastID,
					Limit:   limit,
				},
			)
		}

		extractCursor := func(row sqlc.ListChannelsPaginatedRow) int64 {
			return row.ID
		}

		return sqldb.ExecutePaginatedQuery(
			ctx, s.cfg.QueryCfg, int64(-1), queryFunc,
			extractCursor, handleChannel,
		)
	}, func() {
		edgePoints = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channel view: %w", err)
	}

	return edgePoints, nil
}

// PruneTip returns the block height and hash of the latest block that has been
// used to prune channels in the graph. Knowing the "prune tip" allows callers
// to tell if the graph is currently in sync with the current best known UTXO
// state.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) PruneTip() (*chainhash.Hash, uint32, error) {
	var (
		ctx       = context.TODO()
		tipHash   chainhash.Hash
		tipHeight uint32
	)
	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		pruneTip, err := db.GetPruneTip(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrGraphNeverPruned
		} else if err != nil {
			return fmt.Errorf("unable to fetch prune tip: %w", err)
		}

		tipHash = chainhash.Hash(pruneTip.BlockHash)
		tipHeight = uint32(pruneTip.BlockHeight)

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return nil, 0, err
	}

	return &tipHash, tipHeight, nil
}

// pruneGraphNodes deletes any node in the DB that doesn't have a channel.
//
// NOTE: this prunes nodes across protocol versions. It will never prune the
// source nodes.
func (s *SQLStore) pruneGraphNodes(ctx context.Context,
	db SQLQueries) ([]route.Vertex, error) {

	nodeKeys, err := db.DeleteUnconnectedNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to delete unconnected "+
			"nodes: %w", err)
	}

	prunedNodes := make([]route.Vertex, len(nodeKeys))
	for i, nodeKey := range nodeKeys {
		pub, err := route.NewVertexFromBytes(nodeKey)
		if err != nil {
			return nil, fmt.Errorf("unable to parse pubkey "+
				"from bytes: %w", err)
		}

		prunedNodes[i] = pub
	}

	return prunedNodes, nil
}

// DisconnectBlockAtHeight is used to indicate that the block specified
// by the passed height has been disconnected from the main chain. This
// will "rewind" the graph back to the height below, deleting channels
// that are no longer confirmed from the graph. The prune log will be
// set to the last prune height valid for the remaining chain.
// Channels that were removed from the graph resulting from the
// disconnected block are returned.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) DisconnectBlockAtHeight(height uint32) (
	[]*models.ChannelEdgeInfo, error) {

	ctx := context.TODO()

	var (
		// Every channel having a ShortChannelID starting at 'height'
		// will no longer be confirmed.
		startShortChanID = lnwire.ShortChannelID{
			BlockHeight: height,
		}

		// Delete everything after this height from the db up until the
		// SCID alias range.
		endShortChanID = aliasmgr.StartingAlias

		removedChans []*models.ChannelEdgeInfo

		chanIDStart = channelIDToBytes(startShortChanID.ToUint64())
		chanIDEnd   = channelIDToBytes(endShortChanID.ToUint64())
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		rows, err := db.GetChannelsBySCIDRange(
			ctx, sqlc.GetChannelsBySCIDRangeParams{
				StartScid: chanIDStart,
				EndScid:   chanIDEnd,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to fetch channels: %w", err)
		}

		if len(rows) == 0 {
			// No channels to disconnect, but still clean up prune
			// log.
			return db.DeletePruneLogEntriesInRange(
				ctx, sqlc.DeletePruneLogEntriesInRangeParams{
					StartHeight: int64(height),
					EndHeight: int64(
						endShortChanID.BlockHeight,
					),
				},
			)
		}

		// Batch build all channel edges for disconnection.
		channelEdges, chanIDsToDelete, err := batchBuildChannelInfo(
			ctx, s.cfg, db, rows,
		)
		if err != nil {
			return err
		}

		removedChans = channelEdges

		err = s.deleteChannels(ctx, db, chanIDsToDelete)
		if err != nil {
			return fmt.Errorf("unable to delete channels: %w", err)
		}

		return db.DeletePruneLogEntriesInRange(
			ctx, sqlc.DeletePruneLogEntriesInRangeParams{
				StartHeight: int64(height),
				EndHeight:   int64(endShortChanID.BlockHeight),
			},
		)
	}, func() {
		removedChans = nil
	})
	if err != nil {
		return nil, fmt.Errorf("unable to disconnect block at "+
			"height: %w", err)
	}

	s.cacheMu.Lock()
	for _, channel := range removedChans {
		s.rejectCache.remove(channel.ChannelID)
		s.chanCache.remove(channel.ChannelID)
	}
	s.cacheMu.Unlock()

	return removedChans, nil
}

// AddEdgeProof sets the proof of an existing edge in the graph database.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) AddEdgeProof(scid lnwire.ShortChannelID,
	proof *models.ChannelAuthProof) error {

	var (
		ctx       = context.TODO()
		scidBytes = channelIDToBytes(scid.ToUint64())
	)

	err := s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		res, err := db.AddV1ChannelProof(
			ctx, sqlc.AddV1ChannelProofParams{
				Scid:              scidBytes,
				Node1Signature:    proof.NodeSig1Bytes,
				Node2Signature:    proof.NodeSig2Bytes,
				Bitcoin1Signature: proof.BitcoinSig1Bytes,
				Bitcoin2Signature: proof.BitcoinSig2Bytes,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to add edge proof: %w", err)
		}

		n, err := res.RowsAffected()
		if err != nil {
			return err
		}

		if n == 0 {
			return fmt.Errorf("no rows affected when adding edge "+
				"proof for SCID %v", scid)
		} else if n > 1 {
			return fmt.Errorf("multiple rows affected when adding "+
				"edge proof for SCID %v: %d rows affected",
				scid, n)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return fmt.Errorf("unable to add edge proof: %w", err)
	}

	return nil
}

// PutClosedScid stores a SCID for a closed channel in the database. This is so
// that we can ignore channel announcements that we know to be closed without
// having to validate them and fetch a block.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) PutClosedScid(scid lnwire.ShortChannelID) error {
	var (
		ctx     = context.TODO()
		chanIDB = channelIDToBytes(scid.ToUint64())
	)

	return s.db.ExecTx(ctx, sqldb.WriteTxOpt(), func(db SQLQueries) error {
		return db.InsertClosedChannel(ctx, chanIDB)
	}, sqldb.NoOpReset)
}

// IsClosedScid checks whether a channel identified by the passed in scid is
// closed. This helps avoid having to perform expensive validation checks.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) IsClosedScid(scid lnwire.ShortChannelID) (bool, error) {
	var (
		ctx      = context.TODO()
		isClosed bool
		chanIDB  = channelIDToBytes(scid.ToUint64())
	)
	err := s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		var err error
		isClosed, err = db.IsClosedChannel(ctx, chanIDB)
		if err != nil {
			return fmt.Errorf("unable to fetch closed channel: %w",
				err)
		}

		return nil
	}, sqldb.NoOpReset)
	if err != nil {
		return false, fmt.Errorf("unable to fetch closed channel: %w",
			err)
	}

	return isClosed, nil
}

// GraphSession will provide the call-back with access to a NodeTraverser
// instance which can be used to perform queries against the channel graph.
//
// NOTE: part of the V1Store interface.
func (s *SQLStore) GraphSession(cb func(graph NodeTraverser) error,
	reset func()) error {

	var ctx = context.TODO()

	return s.db.ExecTx(ctx, sqldb.ReadTxOpt(), func(db SQLQueries) error {
		return cb(newSQLNodeTraverser(db, s.cfg.ChainHash))
	}, reset)
}

// sqlNodeTraverser implements the NodeTraverser interface but with a backing
// read only transaction for a consistent view of the graph.
type sqlNodeTraverser struct {
	db    SQLQueries
	chain chainhash.Hash
}

// A compile-time assertion to ensure that sqlNodeTraverser implements the
// NodeTraverser interface.
var _ NodeTraverser = (*sqlNodeTraverser)(nil)

// newSQLNodeTraverser creates a new instance of the sqlNodeTraverser.
func newSQLNodeTraverser(db SQLQueries,
	chain chainhash.Hash) *sqlNodeTraverser {

	return &sqlNodeTraverser{
		db:    db,
		chain: chain,
	}
}

// ForEachNodeDirectedChannel calls the callback for every channel of the given
// node.
//
// NOTE: Part of the NodeTraverser interface.
func (s *sqlNodeTraverser) ForEachNodeDirectedChannel(nodePub route.Vertex,
	cb func(channel *DirectedChannel) error, _ func()) error {

	ctx := context.TODO()

	return forEachNodeDirectedChannel(ctx, s.db, nodePub, cb)
}

// FetchNodeFeatures returns the features of the given node. If the node is
// unknown, assume no additional features are supported.
//
// NOTE: Part of the NodeTraverser interface.
func (s *sqlNodeTraverser) FetchNodeFeatures(nodePub route.Vertex) (
	*lnwire.FeatureVector, error) {

	ctx := context.TODO()

	return fetchNodeFeatures(ctx, s.db, nodePub)
}

// forEachNodeDirectedChannel iterates through all channels of a given
// node, executing the passed callback on the directed edge representing the
// channel and its incoming policy. If the node is not found, no error is
// returned.
func forEachNodeDirectedChannel(ctx context.Context, db SQLQueries,
	nodePub route.Vertex, cb func(channel *DirectedChannel) error) error {

	toNodeCallback := func() route.Vertex {
		return nodePub
	}

	dbID, err := db.GetNodeIDByPubKey(
		ctx, sqlc.GetNodeIDByPubKeyParams{
			Version: int16(lnwire.GossipVersion1),
			PubKey:  nodePub[:],
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("unable to fetch node: %w", err)
	}

	rows, err := db.ListChannelsByNodeID(
		ctx, sqlc.ListChannelsByNodeIDParams{
			Version: int16(lnwire.GossipVersion1),
			NodeID1: dbID,
		},
	)
	if err != nil {
		return fmt.Errorf("unable to fetch channels: %w", err)
	}

	// Exit early if there are no channels for this node so we don't
	// do the unnecessary feature fetching.
	if len(rows) == 0 {
		return nil
	}

	features, err := getNodeFeatures(ctx, db, dbID)
	if err != nil {
		return fmt.Errorf("unable to fetch node features: %w", err)
	}

	for _, row := range rows {
		node1, node2, err := buildNodeVertices(
			row.Node1Pubkey, row.Node2Pubkey,
		)
		if err != nil {
			return fmt.Errorf("unable to build node vertices: %w",
				err)
		}

		edge := buildCacheableChannelInfo(
			row.GraphChannel.Scid, row.GraphChannel.Capacity.Int64,
			node1, node2,
		)

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return err
		}

		p1, p2, err := buildCachedChanPolicies(
			dbPol1, dbPol2, edge.ChannelID, node1, node2,
		)
		if err != nil {
			return err
		}

		// Determine the outgoing and incoming policy for this
		// channel and node combo.
		outPolicy, inPolicy := p1, p2
		if p1 != nil && node2 == nodePub {
			outPolicy, inPolicy = p2, p1
		} else if p2 != nil && node1 != nodePub {
			outPolicy, inPolicy = p2, p1
		}

		var cachedInPolicy *models.CachedEdgePolicy
		if inPolicy != nil {
			cachedInPolicy = inPolicy
			cachedInPolicy.ToNodePubKey = toNodeCallback
			cachedInPolicy.ToNodeFeatures = features
		}

		directedChannel := &DirectedChannel{
			ChannelID:    edge.ChannelID,
			IsNode1:      nodePub == edge.NodeKey1Bytes,
			OtherNode:    edge.NodeKey2Bytes,
			Capacity:     edge.Capacity,
			OutPolicySet: outPolicy != nil,
			InPolicy:     cachedInPolicy,
		}
		if outPolicy != nil {
			outPolicy.InboundFee.WhenSome(func(fee lnwire.Fee) {
				directedChannel.InboundFee = fee
			})
		}

		if nodePub == edge.NodeKey2Bytes {
			directedChannel.OtherNode = edge.NodeKey1Bytes
		}

		if err := cb(directedChannel); err != nil {
			return err
		}
	}

	return nil
}

// forEachNodeCacheable fetches all V1 node IDs and pub keys from the database,
// and executes the provided callback for each node. It does so via pagination
// along with batch loading of the node feature bits.
func forEachNodeCacheable(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, processNode func(nodeID int64, nodePub route.Vertex,
		features *lnwire.FeatureVector) error) error {

	handleNode := func(_ context.Context,
		dbNode sqlc.ListNodeIDsAndPubKeysRow,
		featureBits map[int64][]int) error {

		fv := lnwire.EmptyFeatureVector()
		if features, exists := featureBits[dbNode.ID]; exists {
			for _, bit := range features {
				fv.Set(lnwire.FeatureBit(bit))
			}
		}

		var pub route.Vertex
		copy(pub[:], dbNode.PubKey)

		return processNode(dbNode.ID, pub, fv)
	}

	queryFunc := func(ctx context.Context, lastID int64,
		limit int32) ([]sqlc.ListNodeIDsAndPubKeysRow, error) {

		return db.ListNodeIDsAndPubKeys(
			ctx, sqlc.ListNodeIDsAndPubKeysParams{
				Version: int16(lnwire.GossipVersion1),
				ID:      lastID,
				Limit:   limit,
			},
		)
	}

	extractCursor := func(row sqlc.ListNodeIDsAndPubKeysRow) int64 {
		return row.ID
	}

	collectFunc := func(node sqlc.ListNodeIDsAndPubKeysRow) (int64, error) {
		return node.ID, nil
	}

	batchQueryFunc := func(ctx context.Context,
		nodeIDs []int64) (map[int64][]int, error) {

		return batchLoadNodeFeaturesHelper(ctx, cfg, db, nodeIDs)
	}

	return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
		ctx, cfg, int64(-1), queryFunc, extractCursor, collectFunc,
		batchQueryFunc, handleNode,
	)
}

// forEachNodeChannel iterates through all channels of a node, executing
// the passed callback on each. The call-back is provided with the channel's
// edge information, the outgoing policy and the incoming policy for the
// channel and node combo.
func forEachNodeChannel(ctx context.Context, db SQLQueries,
	cfg *SQLStoreConfig, id int64, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	// Get all the V1 channels for this node.
	rows, err := db.ListChannelsByNodeID(
		ctx, sqlc.ListChannelsByNodeIDParams{
			Version: int16(lnwire.GossipVersion1),
			NodeID1: id,
		},
	)
	if err != nil {
		return fmt.Errorf("unable to fetch channels: %w", err)
	}

	// Collect all the channel and policy IDs.
	var (
		chanIDs   = make([]int64, 0, len(rows))
		policyIDs = make([]int64, 0, 2*len(rows))
	)
	for _, row := range rows {
		chanIDs = append(chanIDs, row.GraphChannel.ID)

		if row.Policy1ID.Valid {
			policyIDs = append(policyIDs, row.Policy1ID.Int64)
		}
		if row.Policy2ID.Valid {
			policyIDs = append(policyIDs, row.Policy2ID.Int64)
		}
	}

	batchData, err := batchLoadChannelData(
		ctx, cfg.QueryCfg, db, chanIDs, policyIDs,
	)
	if err != nil {
		return fmt.Errorf("unable to batch load channel data: %w", err)
	}

	// Call the call-back for each channel and its known policies.
	for _, row := range rows {
		node1, node2, err := buildNodeVertices(
			row.Node1Pubkey, row.Node2Pubkey,
		)
		if err != nil {
			return fmt.Errorf("unable to build node vertices: %w",
				err)
		}

		edge, err := buildEdgeInfoWithBatchData(
			cfg.ChainHash, row.GraphChannel, node1, node2,
			batchData,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel info: %w",
				err)
		}

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return fmt.Errorf("unable to extract channel "+
				"policies: %w", err)
		}

		p1, p2, err := buildChanPoliciesWithBatchData(
			dbPol1, dbPol2, edge.ChannelID, node1, node2, batchData,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel "+
				"policies: %w", err)
		}

		// Determine the outgoing and incoming policy for this
		// channel and node combo.
		p1ToNode := row.GraphChannel.NodeID2
		p2ToNode := row.GraphChannel.NodeID1
		outPolicy, inPolicy := p1, p2
		if (p1 != nil && p1ToNode == id) ||
			(p2 != nil && p2ToNode != id) {

			outPolicy, inPolicy = p2, p1
		}

		if err := cb(edge, outPolicy, inPolicy); err != nil {
			return err
		}
	}

	return nil
}

// updateChanEdgePolicy upserts the channel policy info we have stored for
// a channel we already know of.
func updateChanEdgePolicy(ctx context.Context, tx SQLQueries,
	edge *models.ChannelEdgePolicy) (route.Vertex, route.Vertex, bool,
	error) {

	var (
		node1Pub, node2Pub route.Vertex
		isNode1            bool
		chanIDB            = channelIDToBytes(edge.ChannelID)
	)

	// Check that this edge policy refers to a channel that we already
	// know of. We do this explicitly so that we can return the appropriate
	// ErrEdgeNotFound error if the channel doesn't exist, rather than
	// abort the transaction which would abort the entire batch.
	dbChan, err := tx.GetChannelAndNodesBySCID(
		ctx, sqlc.GetChannelAndNodesBySCIDParams{
			Scid:    chanIDB,
			Version: int16(lnwire.GossipVersion1),
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return node1Pub, node2Pub, false, ErrEdgeNotFound
	} else if err != nil {
		return node1Pub, node2Pub, false, fmt.Errorf("unable to "+
			"fetch channel(%v): %w", edge.ChannelID, err)
	}

	copy(node1Pub[:], dbChan.Node1PubKey)
	copy(node2Pub[:], dbChan.Node2PubKey)

	// Figure out which node this edge is from.
	isNode1 = edge.ChannelFlags&lnwire.ChanUpdateDirection == 0
	nodeID := dbChan.NodeID1
	if !isNode1 {
		nodeID = dbChan.NodeID2
	}

	var (
		inboundBase sql.NullInt64
		inboundRate sql.NullInt64
	)
	edge.InboundFee.WhenSome(func(fee lnwire.Fee) {
		inboundRate = sqldb.SQLInt64(fee.FeeRate)
		inboundBase = sqldb.SQLInt64(fee.BaseFee)
	})

	id, err := tx.UpsertEdgePolicy(ctx, sqlc.UpsertEdgePolicyParams{
		Version:     int16(lnwire.GossipVersion1),
		ChannelID:   dbChan.ID,
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
		return node1Pub, node2Pub, isNode1,
			fmt.Errorf("unable to upsert edge policy: %w", err)
	}

	// Convert the flat extra opaque data into a map of TLV types to
	// values.
	extra, err := marshalExtraOpaqueData(edge.ExtraOpaqueData)
	if err != nil {
		return node1Pub, node2Pub, false, fmt.Errorf("unable to "+
			"marshal extra opaque data: %w", err)
	}

	// Update the channel policy's extra signed fields.
	err = upsertChanPolicyExtraSignedFields(ctx, tx, id, extra)
	if err != nil {
		return node1Pub, node2Pub, false, fmt.Errorf("inserting chan "+
			"policy extra TLVs: %w", err)
	}

	return node1Pub, node2Pub, isNode1, nil
}

// getNodeByPubKey attempts to look up a target node by its public key.
func getNodeByPubKey(ctx context.Context, cfg *sqldb.QueryConfig, db SQLQueries,
	pubKey route.Vertex) (int64, *models.Node, error) {

	dbNode, err := db.GetNodeByPubKey(
		ctx, sqlc.GetNodeByPubKeyParams{
			Version: int16(lnwire.GossipVersion1),
			PubKey:  pubKey[:],
		},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, ErrGraphNodeNotFound
	} else if err != nil {
		return 0, nil, fmt.Errorf("unable to fetch node: %w", err)
	}

	node, err := buildNode(ctx, cfg, db, dbNode)
	if err != nil {
		return 0, nil, fmt.Errorf("unable to build node: %w", err)
	}

	return dbNode.ID, node, nil
}

// buildCacheableChannelInfo builds a models.CachedEdgeInfo instance from the
// provided parameters.
func buildCacheableChannelInfo(scid []byte, capacity int64, node1Pub,
	node2Pub route.Vertex) *models.CachedEdgeInfo {

	return &models.CachedEdgeInfo{
		ChannelID:     byteOrder.Uint64(scid),
		NodeKey1Bytes: node1Pub,
		NodeKey2Bytes: node2Pub,
		Capacity:      btcutil.Amount(capacity),
	}
}

// buildNode constructs a Node instance from the given database node
// record. The node's features, addresses and extra signed fields are also
// fetched from the database and set on the node.
func buildNode(ctx context.Context, cfg *sqldb.QueryConfig, db SQLQueries,
	dbNode sqlc.GraphNode) (*models.Node, error) {

	data, err := batchLoadNodeData(ctx, cfg, db, []int64{dbNode.ID})
	if err != nil {
		return nil, fmt.Errorf("unable to batch load node data: %w",
			err)
	}

	return buildNodeWithBatchData(dbNode, data)
}

// buildNodeWithBatchData builds a models.Node instance
// from the provided sqlc.GraphNode and batchNodeData. If the node does have
// features/addresses/extra fields, then the corresponding fields are expected
// to be present in the batchNodeData.
func buildNodeWithBatchData(dbNode sqlc.GraphNode,
	batchData *batchNodeData) (*models.Node, error) {

	if dbNode.Version != int16(lnwire.GossipVersion1) {
		return nil, fmt.Errorf("unsupported node version: %d",
			dbNode.Version)
	}

	var pub [33]byte
	copy(pub[:], dbNode.PubKey)

	node := models.NewV1ShellNode(pub)

	if len(dbNode.Signature) == 0 {
		return node, nil
	}

	node.AuthSigBytes = dbNode.Signature

	if dbNode.Alias.Valid {
		node.Alias = fn.Some(dbNode.Alias.String)
	}
	if dbNode.LastUpdate.Valid {
		node.LastUpdate = time.Unix(dbNode.LastUpdate.Int64, 0)
	}

	var err error
	if dbNode.Color.Valid {
		nodeColor, err := DecodeHexColor(dbNode.Color.String)
		if err != nil {
			return nil, fmt.Errorf("unable to decode color: %w",
				err)
		}

		node.Color = fn.Some(nodeColor)
	}

	// Use preloaded features.
	if features, exists := batchData.features[dbNode.ID]; exists {
		fv := lnwire.EmptyFeatureVector()
		for _, bit := range features {
			fv.Set(lnwire.FeatureBit(bit))
		}
		node.Features = fv
	}

	// Use preloaded addresses.
	addresses, exists := batchData.addresses[dbNode.ID]
	if exists && len(addresses) > 0 {
		node.Addresses, err = buildNodeAddresses(addresses)
		if err != nil {
			return nil, fmt.Errorf("unable to build addresses "+
				"for node(%d): %w", dbNode.ID, err)
		}
	}

	// Use preloaded extra fields.
	if extraFields, exists := batchData.extraFields[dbNode.ID]; exists {
		recs, err := lnwire.CustomRecords(extraFields).Serialize()
		if err != nil {
			return nil, fmt.Errorf("unable to serialize extra "+
				"signed fields: %w", err)
		}
		if len(recs) != 0 {
			node.ExtraOpaqueData = recs
		}
	}

	return node, nil
}

// forEachNodeInBatch fetches all nodes in the provided batch, builds them
// with the preloaded data, and executes the provided callback for each node.
func forEachNodeInBatch(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, nodes []sqlc.GraphNode,
	cb func(dbID int64, node *models.Node) error) error {

	// Extract node IDs for batch loading.
	nodeIDs := make([]int64, len(nodes))
	for i, node := range nodes {
		nodeIDs[i] = node.ID
	}

	// Batch load all related data for this page.
	batchData, err := batchLoadNodeData(ctx, cfg, db, nodeIDs)
	if err != nil {
		return fmt.Errorf("unable to batch load node data: %w", err)
	}

	for _, dbNode := range nodes {
		node, err := buildNodeWithBatchData(dbNode, batchData)
		if err != nil {
			return fmt.Errorf("unable to build node(id=%d): %w",
				dbNode.ID, err)
		}

		if err := cb(dbNode.ID, node); err != nil {
			return fmt.Errorf("callback failed for node(id=%d): %w",
				dbNode.ID, err)
		}
	}

	return nil
}

// getNodeFeatures fetches the feature bits and constructs the feature vector
// for a node with the given DB ID.
func getNodeFeatures(ctx context.Context, db SQLQueries,
	nodeID int64) (*lnwire.FeatureVector, error) {

	rows, err := db.GetNodeFeatures(ctx, nodeID)
	if err != nil {
		return nil, fmt.Errorf("unable to get node(%d) features: %w",
			nodeID, err)
	}

	features := lnwire.EmptyFeatureVector()
	for _, feature := range rows {
		features.Set(lnwire.FeatureBit(feature.FeatureBit))
	}

	return features, nil
}

// upsertNodeAncillaryData updates the node's features, addresses, and extra
// signed fields. This is common logic shared by upsertNode and
// upsertSourceNode.
func upsertNodeAncillaryData(ctx context.Context, db SQLQueries,
	nodeID int64, node *models.Node) error {

	// Update the node's features.
	err := upsertNodeFeatures(ctx, db, nodeID, node.Features)
	if err != nil {
		return fmt.Errorf("inserting node features: %w", err)
	}

	// Update the node's addresses.
	err = upsertNodeAddresses(ctx, db, nodeID, node.Addresses)
	if err != nil {
		return fmt.Errorf("inserting node addresses: %w", err)
	}

	// Convert the flat extra opaque data into a map of TLV types to
	// values.
	extra, err := marshalExtraOpaqueData(node.ExtraOpaqueData)
	if err != nil {
		return fmt.Errorf("unable to marshal extra opaque data: %w",
			err)
	}

	// Update the node's extra signed fields.
	err = upsertNodeExtraSignedFields(ctx, db, nodeID, extra)
	if err != nil {
		return fmt.Errorf("inserting node extra TLVs: %w", err)
	}

	return nil
}

// populateNodeParams populates the common node parameters from a models.Node.
// This is a helper for building UpsertNodeParams and UpsertSourceNodeParams.
func populateNodeParams(node *models.Node,
	setParams func(lastUpdate sql.NullInt64, alias,
		colorStr sql.NullString, signature []byte)) error {

	if !node.HaveAnnouncement() {
		return nil
	}

	switch node.Version {
	case lnwire.GossipVersion1:
		lastUpdate := sqldb.SQLInt64(node.LastUpdate.Unix())
		var alias, colorStr sql.NullString

		node.Color.WhenSome(func(rgba color.RGBA) {
			colorStr = sqldb.SQLStrValid(EncodeHexColor(rgba))
		})
		node.Alias.WhenSome(func(s string) {
			alias = sqldb.SQLStrValid(s)
		})

		setParams(lastUpdate, alias, colorStr, node.AuthSigBytes)

	case lnwire.GossipVersion2:
		// No-op for now.

	default:
		return fmt.Errorf("unknown gossip version: %d", node.Version)
	}

	return nil
}

// buildNodeUpsertParams builds the parameters for upserting a node using the
// strict UpsertNode query (requires timestamp to be increasing).
func buildNodeUpsertParams(node *models.Node) (sqlc.UpsertNodeParams, error) {
	params := sqlc.UpsertNodeParams{
		Version: int16(lnwire.GossipVersion1),
		PubKey:  node.PubKeyBytes[:],
	}

	err := populateNodeParams(
		node, func(lastUpdate sql.NullInt64, alias,
			colorStr sql.NullString,
			signature []byte) {

			params.LastUpdate = lastUpdate
			params.Alias = alias
			params.Color = colorStr
			params.Signature = signature
		})

	return params, err
}

// buildSourceNodeUpsertParams builds the parameters for upserting the source
// node using the lenient UpsertSourceNode query (allows same timestamp).
func buildSourceNodeUpsertParams(node *models.Node) (
	sqlc.UpsertSourceNodeParams, error) {

	params := sqlc.UpsertSourceNodeParams{
		Version: int16(lnwire.GossipVersion1),
		PubKey:  node.PubKeyBytes[:],
	}

	err := populateNodeParams(
		node, func(lastUpdate sql.NullInt64, alias,
			colorStr sql.NullString, signature []byte) {

			params.LastUpdate = lastUpdate
			params.Alias = alias
			params.Color = colorStr
			params.Signature = signature
		},
	)

	return params, err
}

// upsertSourceNode upserts the source node record into the database using a
// less strict upsert that allows updates even when the timestamp hasn't
// changed. This is necessary to handle concurrent updates to our own node
// during startup and runtime. The node's features, addresses and extra TLV
// types are also updated. The node's DB ID is returned.
func upsertSourceNode(ctx context.Context, db SQLQueries,
	node *models.Node) (int64, error) {

	params, err := buildSourceNodeUpsertParams(node)
	if err != nil {
		return 0, err
	}

	nodeID, err := db.UpsertSourceNode(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("upserting source node(%x): %w",
			node.PubKeyBytes, err)
	}

	// We can exit here if we don't have the announcement yet.
	if !node.HaveAnnouncement() {
		return nodeID, nil
	}

	// Update the ancillary node data (features, addresses, extra fields).
	err = upsertNodeAncillaryData(ctx, db, nodeID, node)
	if err != nil {
		return 0, err
	}

	return nodeID, nil
}

// upsertNode upserts the node record into the database. If the node already
// exists, then the node's information is updated. If the node doesn't exist,
// then a new node is created. The node's features, addresses and extra TLV
// types are also updated. The node's DB ID is returned.
func upsertNode(ctx context.Context, db SQLQueries,
	node *models.Node) (int64, error) {

	params, err := buildNodeUpsertParams(node)
	if err != nil {
		return 0, err
	}

	nodeID, err := db.UpsertNode(ctx, params)
	if err != nil {
		return 0, fmt.Errorf("upserting node(%x): %w", node.PubKeyBytes,
			err)
	}

	// We can exit here if we don't have the announcement yet.
	if !node.HaveAnnouncement() {
		return nodeID, nil
	}

	// Update the ancillary node data (features, addresses, extra fields).
	err = upsertNodeAncillaryData(ctx, db, nodeID, node)
	if err != nil {
		return 0, err
	}

	return nodeID, nil
}

// upsertNodeFeatures updates the node's features node_features table. This
// includes deleting any feature bits no longer present and inserting any new
// feature bits. If the feature bit does not yet exist in the features table,
// then an entry is created in that table first.
func upsertNodeFeatures(ctx context.Context, db SQLQueries, nodeID int64,
	features *lnwire.FeatureVector) error {

	// Get any existing features for the node.
	existingFeatures, err := db.GetNodeFeatures(ctx, nodeID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// Copy the nodes latest set of feature bits.
	newFeatures := make(map[int32]struct{})
	if features != nil {
		for feature := range features.Features() {
			newFeatures[int32(feature)] = struct{}{}
		}
	}

	// For any current feature that already exists in the DB, remove it from
	// the in-memory map. For any existing feature that does not exist in
	// the in-memory map, delete it from the database.
	for _, feature := range existingFeatures {
		// The feature is still present, so there are no updates to be
		// made.
		if _, ok := newFeatures[feature.FeatureBit]; ok {
			delete(newFeatures, feature.FeatureBit)
			continue
		}

		// The feature is no longer present, so we remove it from the
		// database.
		err := db.DeleteNodeFeature(ctx, sqlc.DeleteNodeFeatureParams{
			NodeID:     nodeID,
			FeatureBit: feature.FeatureBit,
		})
		if err != nil {
			return fmt.Errorf("unable to delete node(%d) "+
				"feature(%v): %w", nodeID, feature.FeatureBit,
				err)
		}
	}

	// Any remaining entries in newFeatures are new features that need to be
	// added to the database for the first time.
	for feature := range newFeatures {
		err = db.InsertNodeFeature(ctx, sqlc.InsertNodeFeatureParams{
			NodeID:     nodeID,
			FeatureBit: feature,
		})
		if err != nil {
			return fmt.Errorf("unable to insert node(%d) "+
				"feature(%v): %w", nodeID, feature, err)
		}
	}

	return nil
}

// fetchNodeFeatures fetches the features for a node with the given public key.
func fetchNodeFeatures(ctx context.Context, queries SQLQueries,
	nodePub route.Vertex) (*lnwire.FeatureVector, error) {

	rows, err := queries.GetNodeFeaturesByPubKey(
		ctx, sqlc.GetNodeFeaturesByPubKeyParams{
			PubKey:  nodePub[:],
			Version: int16(lnwire.GossipVersion1),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("unable to get node(%s) features: %w",
			nodePub, err)
	}

	features := lnwire.EmptyFeatureVector()
	for _, bit := range rows {
		features.Set(lnwire.FeatureBit(bit))
	}

	return features, nil
}

// dbAddressType is an enum type that represents the different address types
// that we store in the node_addresses table. The address type determines how
// the address is to be serialised/deserialize.
type dbAddressType uint8

const (
	addressTypeIPv4   dbAddressType = 1
	addressTypeIPv6   dbAddressType = 2
	addressTypeTorV2  dbAddressType = 3
	addressTypeTorV3  dbAddressType = 4
	addressTypeDNS    dbAddressType = 5
	addressTypeOpaque dbAddressType = math.MaxInt8
)

// collectAddressRecords collects the addresses from the provided
// net.Addr slice and returns a map of dbAddressType to a slice of address
// strings.
func collectAddressRecords(addresses []net.Addr) (map[dbAddressType][]string,
	error) {

	// Copy the nodes latest set of addresses.
	newAddresses := map[dbAddressType][]string{
		addressTypeIPv4:   {},
		addressTypeIPv6:   {},
		addressTypeTorV2:  {},
		addressTypeTorV3:  {},
		addressTypeDNS:    {},
		addressTypeOpaque: {},
	}
	addAddr := func(t dbAddressType, addr net.Addr) {
		newAddresses[t] = append(newAddresses[t], addr.String())
	}

	for _, address := range addresses {
		switch addr := address.(type) {
		case *net.TCPAddr:
			if ip4 := addr.IP.To4(); ip4 != nil {
				addAddr(addressTypeIPv4, addr)
			} else if ip6 := addr.IP.To16(); ip6 != nil {
				addAddr(addressTypeIPv6, addr)
			} else {
				return nil, fmt.Errorf("unhandled IP "+
					"address: %v", addr)
			}

		case *tor.OnionAddr:
			switch len(addr.OnionService) {
			case tor.V2Len:
				addAddr(addressTypeTorV2, addr)
			case tor.V3Len:
				addAddr(addressTypeTorV3, addr)
			default:
				return nil, fmt.Errorf("invalid length for " +
					"a tor address")
			}

		case *lnwire.DNSAddress:
			addAddr(addressTypeDNS, addr)

		case *lnwire.OpaqueAddrs:
			addAddr(addressTypeOpaque, addr)

		default:
			return nil, fmt.Errorf("unhandled address type: %T",
				addr)
		}
	}

	return newAddresses, nil
}

// upsertNodeAddresses updates the node's addresses in the database. This
// includes deleting any existing addresses and inserting the new set of
// addresses. The deletion is necessary since the ordering of the addresses may
// change, and we need to ensure that the database reflects the latest set of
// addresses so that at the time of reconstructing the node announcement, the
// order is preserved and the signature over the message remains valid.
func upsertNodeAddresses(ctx context.Context, db SQLQueries, nodeID int64,
	addresses []net.Addr) error {

	// Delete any existing addresses for the node. This is required since
	// even if the new set of addresses is the same, the ordering may have
	// changed for a given address type.
	err := db.DeleteNodeAddresses(ctx, nodeID)
	if err != nil {
		return fmt.Errorf("unable to delete node(%d) addresses: %w",
			nodeID, err)
	}

	newAddresses, err := collectAddressRecords(addresses)
	if err != nil {
		return err
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
				return fmt.Errorf("unable to insert "+
					"node(%d) address(%v): %w", nodeID,
					addr, err)
			}
		}
	}

	return nil
}

// getNodeAddresses fetches the addresses for a node with the given DB ID.
func getNodeAddresses(ctx context.Context, db SQLQueries, id int64) ([]net.Addr,
	error) {

	// GetNodeAddresses ensures that the addresses for a given type are
	// returned in the same order as they were inserted.
	rows, err := db.GetNodeAddresses(ctx, id)
	if err != nil {
		return nil, err
	}

	addresses := make([]net.Addr, 0, len(rows))
	for _, row := range rows {
		address := row.Address

		addr, err := parseAddress(dbAddressType(row.Type), address)
		if err != nil {
			return nil, fmt.Errorf("unable to parse address "+
				"for node(%d): %v: %w", id, address, err)
		}

		addresses = append(addresses, addr)
	}

	// If we have no addresses, then we'll return nil instead of an
	// empty slice.
	if len(addresses) == 0 {
		addresses = nil
	}

	return addresses, nil
}

// upsertNodeExtraSignedFields updates the node's extra signed fields in the
// database. This includes updating any existing types, inserting any new types,
// and deleting any types that are no longer present.
func upsertNodeExtraSignedFields(ctx context.Context, db SQLQueries,
	nodeID int64, extraFields map[uint64][]byte) error {

	// Get any existing extra signed fields for the node.
	existingFields, err := db.GetExtraNodeTypes(ctx, nodeID)
	if err != nil {
		return err
	}

	// Make a lookup map of the existing field types so that we can use it
	// to keep track of any fields we should delete.
	m := make(map[uint64]bool)
	for _, field := range existingFields {
		m[uint64(field.Type)] = true
	}

	// For all the new fields, we'll upsert them and remove them from the
	// map of existing fields.
	for tlvType, value := range extraFields {
		err = db.UpsertNodeExtraType(
			ctx, sqlc.UpsertNodeExtraTypeParams{
				NodeID: nodeID,
				Type:   int64(tlvType),
				Value:  value,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to upsert node(%d) extra "+
				"signed field(%v): %w", nodeID, tlvType, err)
		}

		// Remove the field from the map of existing fields if it was
		// present.
		delete(m, tlvType)
	}

	// For all the fields that are left in the map of existing fields, we'll
	// delete them as they are no longer present in the new set of fields.
	for tlvType := range m {
		err = db.DeleteExtraNodeType(
			ctx, sqlc.DeleteExtraNodeTypeParams{
				NodeID: nodeID,
				Type:   int64(tlvType),
			},
		)
		if err != nil {
			return fmt.Errorf("unable to delete node(%d) extra "+
				"signed field(%v): %w", nodeID, tlvType, err)
		}
	}

	return nil
}

// srcNodeInfo holds the information about the source node of the graph.
type srcNodeInfo struct {
	// id is the DB level ID of the source node entry in the "nodes" table.
	id int64

	// pub is the public key of the source node.
	pub route.Vertex
}

// sourceNode returns the DB node ID and pub key of the source node for the
// specified protocol version.
func (s *SQLStore) getSourceNode(ctx context.Context, db SQLQueries,
	version lnwire.GossipVersion) (int64, route.Vertex, error) {

	s.srcNodeMu.Lock()
	defer s.srcNodeMu.Unlock()

	// If we already have the source node ID and pub key cached, then
	// return them.
	if info, ok := s.srcNodes[version]; ok {
		return info.id, info.pub, nil
	}

	var pubKey route.Vertex

	nodes, err := db.GetSourceNodesByVersion(ctx, int16(version))
	if err != nil {
		return 0, pubKey, fmt.Errorf("unable to fetch source node: %w",
			err)
	}

	if len(nodes) == 0 {
		return 0, pubKey, ErrSourceNodeNotSet
	} else if len(nodes) > 1 {
		return 0, pubKey, fmt.Errorf("multiple source nodes for "+
			"protocol %s found", version)
	}

	copy(pubKey[:], nodes[0].PubKey)

	s.srcNodes[version] = &srcNodeInfo{
		id:  nodes[0].NodeID,
		pub: pubKey,
	}

	return nodes[0].NodeID, pubKey, nil
}

// marshalExtraOpaqueData takes a flat byte slice parses it as a TLV stream.
// This then produces a map from TLV type to value. If the input is not a
// valid TLV stream, then an error is returned.
func marshalExtraOpaqueData(data []byte) (map[uint64][]byte, error) {
	r := bytes.NewReader(data)

	tlvStream, err := tlv.NewStream()
	if err != nil {
		return nil, err
	}

	// Since ExtraOpaqueData is provided by a potentially malicious peer,
	// pass it into the P2P decoding variant.
	parsedTypes, err := tlvStream.DecodeWithParsedTypesP2P(r)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrParsingExtraTLVBytes, err)
	}
	if len(parsedTypes) == 0 {
		return nil, nil
	}

	records := make(map[uint64][]byte)
	for k, v := range parsedTypes {
		records[uint64(k)] = v
	}

	return records, nil
}

// insertChannel inserts a new channel record into the database.
func insertChannel(ctx context.Context, db SQLQueries,
	edge *models.ChannelEdgeInfo) error {

	// Make sure that at least a "shell" entry for each node is present in
	// the nodes table.
	node1DBID, err := maybeCreateShellNode(ctx, db, edge.NodeKey1Bytes)
	if err != nil {
		return fmt.Errorf("unable to create shell node: %w", err)
	}

	node2DBID, err := maybeCreateShellNode(ctx, db, edge.NodeKey2Bytes)
	if err != nil {
		return fmt.Errorf("unable to create shell node: %w", err)
	}

	var capacity sql.NullInt64
	if edge.Capacity != 0 {
		capacity = sqldb.SQLInt64(int64(edge.Capacity))
	}

	createParams := sqlc.CreateChannelParams{
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
	dbChanID, err := db.CreateChannel(ctx, createParams)
	if err != nil {
		return err
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
			return fmt.Errorf("unable to insert channel(%d) "+
				"feature(%v): %w", dbChanID, feature, err)
		}
	}

	// Finally, insert any extra TLV fields in the channel announcement.
	extra, err := marshalExtraOpaqueData(edge.ExtraOpaqueData)
	if err != nil {
		return fmt.Errorf("unable to marshal extra opaque data: %w",
			err)
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
			return fmt.Errorf("unable to upsert channel(%d) "+
				"extra signed field(%v): %w", edge.ChannelID,
				tlvType, err)
		}
	}

	return nil
}

// maybeCreateShellNode checks if a shell node entry exists for the
// given public key. If it does not exist, then a new shell node entry is
// created. The ID of the node is returned. A shell node only has a protocol
// version and public key persisted.
func maybeCreateShellNode(ctx context.Context, db SQLQueries,
	pubKey route.Vertex) (int64, error) {

	dbNode, err := db.GetNodeByPubKey(
		ctx, sqlc.GetNodeByPubKeyParams{
			PubKey:  pubKey[:],
			Version: int16(lnwire.GossipVersion1),
		},
	)
	// The node exists. Return the ID.
	if err == nil {
		return dbNode.ID, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// Otherwise, the node does not exist, so we create a shell entry for
	// it.
	id, err := db.UpsertNode(ctx, sqlc.UpsertNodeParams{
		Version: int16(lnwire.GossipVersion1),
		PubKey:  pubKey[:],
	})
	if err != nil {
		return 0, fmt.Errorf("unable to create shell node: %w", err)
	}

	return id, nil
}

// upsertChanPolicyExtraSignedFields updates the policy's extra signed fields in
// the database. This includes deleting any existing types and then inserting
// the new types.
func upsertChanPolicyExtraSignedFields(ctx context.Context, db SQLQueries,
	chanPolicyID int64, extraFields map[uint64][]byte) error {

	// Delete all existing extra signed fields for the channel policy.
	err := db.DeleteChannelPolicyExtraTypes(ctx, chanPolicyID)
	if err != nil {
		return fmt.Errorf("unable to delete "+
			"existing policy extra signed fields for policy %d: %w",
			chanPolicyID, err)
	}

	// Insert all new extra signed fields for the channel policy.
	for tlvType, value := range extraFields {
		err = db.UpsertChanPolicyExtraType(
			ctx, sqlc.UpsertChanPolicyExtraTypeParams{
				ChannelPolicyID: chanPolicyID,
				Type:            int64(tlvType),
				Value:           value,
			},
		)
		if err != nil {
			return fmt.Errorf("unable to insert "+
				"channel_policy(%d) extra signed field(%v): %w",
				chanPolicyID, tlvType, err)
		}
	}

	return nil
}

// getAndBuildEdgeInfo builds a models.ChannelEdgeInfo instance from the
// provided dbChanRow and also fetches any other required information
// to construct the edge info.
func getAndBuildEdgeInfo(ctx context.Context, cfg *SQLStoreConfig,
	db SQLQueries, dbChan sqlc.GraphChannel, node1,
	node2 route.Vertex) (*models.ChannelEdgeInfo, error) {

	data, err := batchLoadChannelData(
		ctx, cfg.QueryCfg, db, []int64{dbChan.ID}, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load channel data: %w",
			err)
	}

	return buildEdgeInfoWithBatchData(
		cfg.ChainHash, dbChan, node1, node2, data,
	)
}

// buildEdgeInfoWithBatchData builds edge info using pre-loaded batch data.
func buildEdgeInfoWithBatchData(chain chainhash.Hash,
	dbChan sqlc.GraphChannel, node1, node2 route.Vertex,
	batchData *batchChannelData) (*models.ChannelEdgeInfo, error) {

	if dbChan.Version != int16(lnwire.GossipVersion1) {
		return nil, fmt.Errorf("unsupported channel version: %d",
			dbChan.Version)
	}

	// Use pre-loaded features and extras types.
	fv := lnwire.EmptyFeatureVector()
	if features, exists := batchData.chanfeatures[dbChan.ID]; exists {
		for _, bit := range features {
			fv.Set(lnwire.FeatureBit(bit))
		}
	}

	var extras map[uint64][]byte
	channelExtras, exists := batchData.chanExtraTypes[dbChan.ID]
	if exists {
		extras = channelExtras
	} else {
		extras = make(map[uint64][]byte)
	}

	op, err := wire.NewOutPointFromString(dbChan.Outpoint)
	if err != nil {
		return nil, err
	}

	recs, err := lnwire.CustomRecords(extras).Serialize()
	if err != nil {
		return nil, fmt.Errorf("unable to serialize extra signed "+
			"fields: %w", err)
	}
	if recs == nil {
		recs = make([]byte, 0)
	}

	var btcKey1, btcKey2 route.Vertex
	copy(btcKey1[:], dbChan.BitcoinKey1)
	copy(btcKey2[:], dbChan.BitcoinKey2)

	channel := &models.ChannelEdgeInfo{
		ChainHash:        chain,
		ChannelID:        byteOrder.Uint64(dbChan.Scid),
		NodeKey1Bytes:    node1,
		NodeKey2Bytes:    node2,
		BitcoinKey1Bytes: btcKey1,
		BitcoinKey2Bytes: btcKey2,
		ChannelPoint:     *op,
		Capacity:         btcutil.Amount(dbChan.Capacity.Int64),
		Features:         fv,
		ExtraOpaqueData:  recs,
	}

	// We always set all the signatures at the same time, so we can
	// safely check if one signature is present to determine if we have the
	// rest of the signatures for the auth proof.
	if len(dbChan.Bitcoin1Signature) > 0 {
		channel.AuthProof = &models.ChannelAuthProof{
			NodeSig1Bytes:    dbChan.Node1Signature,
			NodeSig2Bytes:    dbChan.Node2Signature,
			BitcoinSig1Bytes: dbChan.Bitcoin1Signature,
			BitcoinSig2Bytes: dbChan.Bitcoin2Signature,
		}
	}

	return channel, nil
}

// buildNodeVertices is a helper that converts raw node public keys
// into route.Vertex instances.
func buildNodeVertices(node1Pub, node2Pub []byte) (route.Vertex,
	route.Vertex, error) {

	node1Vertex, err := route.NewVertexFromBytes(node1Pub)
	if err != nil {
		return route.Vertex{}, route.Vertex{}, fmt.Errorf("unable to "+
			"create vertex from node1 pubkey: %w", err)
	}

	node2Vertex, err := route.NewVertexFromBytes(node2Pub)
	if err != nil {
		return route.Vertex{}, route.Vertex{}, fmt.Errorf("unable to "+
			"create vertex from node2 pubkey: %w", err)
	}

	return node1Vertex, node2Vertex, nil
}

// getAndBuildChanPolicies uses the given sqlc.GraphChannelPolicy and also
// retrieves all the extra info required to build the complete
// models.ChannelEdgePolicy types. It returns two policies, which may be nil if
// the provided sqlc.GraphChannelPolicy records are nil.
func getAndBuildChanPolicies(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, dbPol1, dbPol2 *sqlc.GraphChannelPolicy,
	channelID uint64, node1, node2 route.Vertex) (*models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	if dbPol1 == nil && dbPol2 == nil {
		return nil, nil, nil
	}

	var policyIDs = make([]int64, 0, 2)
	if dbPol1 != nil {
		policyIDs = append(policyIDs, dbPol1.ID)
	}
	if dbPol2 != nil {
		policyIDs = append(policyIDs, dbPol2.ID)
	}

	batchData, err := batchLoadChannelData(ctx, cfg, db, nil, policyIDs)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to batch load channel "+
			"data: %w", err)
	}

	pol1, err := buildChanPolicyWithBatchData(
		dbPol1, channelID, node2, batchData,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build policy1: %w", err)
	}

	pol2, err := buildChanPolicyWithBatchData(
		dbPol2, channelID, node1, batchData,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build policy2: %w", err)
	}

	return pol1, pol2, nil
}

// buildCachedChanPolicies builds models.CachedEdgePolicy instances from the
// provided sqlc.GraphChannelPolicy objects. If a provided policy is nil,
// then nil is returned for it.
func buildCachedChanPolicies(dbPol1, dbPol2 *sqlc.GraphChannelPolicy,
	channelID uint64, node1, node2 route.Vertex) (*models.CachedEdgePolicy,
	*models.CachedEdgePolicy, error) {

	var p1, p2 *models.CachedEdgePolicy
	if dbPol1 != nil {
		policy1, err := buildChanPolicy(*dbPol1, channelID, nil, node2)
		if err != nil {
			return nil, nil, err
		}

		p1 = models.NewCachedPolicy(policy1)
	}
	if dbPol2 != nil {
		policy2, err := buildChanPolicy(*dbPol2, channelID, nil, node1)
		if err != nil {
			return nil, nil, err
		}

		p2 = models.NewCachedPolicy(policy2)
	}

	return p1, p2, nil
}

// buildChanPolicy builds a models.ChannelEdgePolicy instance from the
// provided sqlc.GraphChannelPolicy and other required information.
func buildChanPolicy(dbPolicy sqlc.GraphChannelPolicy, channelID uint64,
	extras map[uint64][]byte,
	toNode route.Vertex) (*models.ChannelEdgePolicy, error) {

	recs, err := lnwire.CustomRecords(extras).Serialize()
	if err != nil {
		return nil, fmt.Errorf("unable to serialize extra signed "+
			"fields: %w", err)
	}

	var inboundFee fn.Option[lnwire.Fee]
	if dbPolicy.InboundFeeRateMilliMsat.Valid ||
		dbPolicy.InboundBaseFeeMsat.Valid {

		inboundFee = fn.Some(lnwire.Fee{
			BaseFee: int32(dbPolicy.InboundBaseFeeMsat.Int64),
			FeeRate: int32(dbPolicy.InboundFeeRateMilliMsat.Int64),
		})
	}

	return &models.ChannelEdgePolicy{
		SigBytes:  dbPolicy.Signature,
		ChannelID: channelID,
		LastUpdate: time.Unix(
			dbPolicy.LastUpdate.Int64, 0,
		),
		MessageFlags: sqldb.ExtractSqlInt16[lnwire.ChanUpdateMsgFlags](
			dbPolicy.MessageFlags,
		),
		ChannelFlags: sqldb.ExtractSqlInt16[lnwire.ChanUpdateChanFlags](
			dbPolicy.ChannelFlags,
		),
		TimeLockDelta: uint16(dbPolicy.Timelock),
		MinHTLC: lnwire.MilliSatoshi(
			dbPolicy.MinHtlcMsat,
		),
		MaxHTLC: lnwire.MilliSatoshi(
			dbPolicy.MaxHtlcMsat.Int64,
		),
		FeeBaseMSat: lnwire.MilliSatoshi(
			dbPolicy.BaseFeeMsat,
		),
		FeeProportionalMillionths: lnwire.MilliSatoshi(dbPolicy.FeePpm),
		ToNode:                    toNode,
		InboundFee:                inboundFee,
		ExtraOpaqueData:           recs,
	}, nil
}

// extractChannelPolicies extracts the sqlc.GraphChannelPolicy records from the give
// row which is expected to be a sqlc type that contains channel policy
// information. It returns two policies, which may be nil if the policy
// information is not present in the row.
//
//nolint:ll,dupl,funlen
func extractChannelPolicies(row any) (*sqlc.GraphChannelPolicy,
	*sqlc.GraphChannelPolicy, error) {

	var policy1, policy2 *sqlc.GraphChannelPolicy
	switch r := row.(type) {
	case sqlc.ListChannelsWithPoliciesForCachePaginatedRow:
		if r.Policy1Timelock.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
			}
		}
		if r.Policy2Timelock.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
			}
		}

		return policy1, policy2, nil

	case sqlc.GetChannelsBySCIDWithPoliciesRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.GetChannelByOutpointWithPoliciesRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.GetChannelBySCIDWithPoliciesRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.GetChannelsByPolicyLastUpdateRangeRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.ListChannelsForNodeIDsRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.ListChannelsByNodeIDRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.ListChannelsWithPoliciesPaginatedRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	case sqlc.GetChannelsByIDsRow:
		if r.Policy1ID.Valid {
			policy1 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy1ID.Int64,
				Version:                 r.Policy1Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy1NodeID.Int64,
				Timelock:                r.Policy1Timelock.Int32,
				FeePpm:                  r.Policy1FeePpm.Int64,
				BaseFeeMsat:             r.Policy1BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy1MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy1MaxHtlcMsat,
				LastUpdate:              r.Policy1LastUpdate,
				InboundBaseFeeMsat:      r.Policy1InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy1InboundFeeRateMilliMsat,
				Disabled:                r.Policy1Disabled,
				MessageFlags:            r.Policy1MessageFlags,
				ChannelFlags:            r.Policy1ChannelFlags,
				Signature:               r.Policy1Signature,
			}
		}
		if r.Policy2ID.Valid {
			policy2 = &sqlc.GraphChannelPolicy{
				ID:                      r.Policy2ID.Int64,
				Version:                 r.Policy2Version.Int16,
				ChannelID:               r.GraphChannel.ID,
				NodeID:                  r.Policy2NodeID.Int64,
				Timelock:                r.Policy2Timelock.Int32,
				FeePpm:                  r.Policy2FeePpm.Int64,
				BaseFeeMsat:             r.Policy2BaseFeeMsat.Int64,
				MinHtlcMsat:             r.Policy2MinHtlcMsat.Int64,
				MaxHtlcMsat:             r.Policy2MaxHtlcMsat,
				LastUpdate:              r.Policy2LastUpdate,
				InboundBaseFeeMsat:      r.Policy2InboundBaseFeeMsat,
				InboundFeeRateMilliMsat: r.Policy2InboundFeeRateMilliMsat,
				Disabled:                r.Policy2Disabled,
				MessageFlags:            r.Policy2MessageFlags,
				ChannelFlags:            r.Policy2ChannelFlags,
				Signature:               r.Policy2Signature,
			}
		}

		return policy1, policy2, nil

	default:
		return nil, nil, fmt.Errorf("unexpected row type in "+
			"extractChannelPolicies: %T", r)
	}
}

// channelIDToBytes converts a channel ID (SCID) to a byte array
// representation.
func channelIDToBytes(channelID uint64) []byte {
	var chanIDB [8]byte
	byteOrder.PutUint64(chanIDB[:], channelID)

	return chanIDB[:]
}

// buildNodeAddresses converts a slice of nodeAddress into a slice of net.Addr.
func buildNodeAddresses(addresses []nodeAddress) ([]net.Addr, error) {
	if len(addresses) == 0 {
		return nil, nil
	}

	result := make([]net.Addr, 0, len(addresses))
	for _, addr := range addresses {
		netAddr, err := parseAddress(addr.addrType, addr.address)
		if err != nil {
			return nil, fmt.Errorf("unable to parse address %s "+
				"of type %d: %w", addr.address, addr.addrType,
				err)
		}
		if netAddr != nil {
			result = append(result, netAddr)
		}
	}

	// If we have no valid addresses, return nil instead of empty slice.
	if len(result) == 0 {
		return nil, nil
	}

	return result, nil
}

// parseAddress parses the given address string based on the address type
// and returns a net.Addr instance. It supports IPv4, IPv6, Tor v2, Tor v3,
// and opaque addresses.
func parseAddress(addrType dbAddressType, address string) (net.Addr, error) {
	switch addrType {
	case addressTypeIPv4:
		tcp, err := net.ResolveTCPAddr("tcp4", address)
		if err != nil {
			return nil, err
		}

		tcp.IP = tcp.IP.To4()

		return tcp, nil

	case addressTypeIPv6:
		tcp, err := net.ResolveTCPAddr("tcp6", address)
		if err != nil {
			return nil, err
		}

		return tcp, nil

	case addressTypeTorV3, addressTypeTorV2:
		service, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("unable to split tor "+
				"address: %v", address)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}

		return &tor.OnionAddr{
			OnionService: service,
			Port:         port,
		}, nil

	case addressTypeDNS:
		hostname, portStr, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("unable to split DNS "+
				"address: %v", address)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, err
		}

		return &lnwire.DNSAddress{
			Hostname: hostname,
			Port:     uint16(port),
		}, nil

	case addressTypeOpaque:
		opaque, err := hex.DecodeString(address)
		if err != nil {
			return nil, fmt.Errorf("unable to decode opaque "+
				"address: %v", address)
		}

		return &lnwire.OpaqueAddrs{
			Payload: opaque,
		}, nil

	default:
		return nil, fmt.Errorf("unknown address type: %v", addrType)
	}
}

// batchNodeData holds all the related data for a batch of nodes.
type batchNodeData struct {
	// features is a map from a DB node ID to the feature bits for that
	// node.
	features map[int64][]int

	// addresses is a map from a DB node ID to the node's addresses.
	addresses map[int64][]nodeAddress

	// extraFields is a map from a DB node ID to the extra signed fields
	// for that node.
	extraFields map[int64]map[uint64][]byte
}

// nodeAddress holds the address type, position and address string for a
// node. This is used to batch the fetching of node addresses.
type nodeAddress struct {
	addrType dbAddressType
	position int32
	address  string
}

// batchLoadNodeData loads all related data for a batch of node IDs using the
// provided SQLQueries interface. It returns a batchNodeData instance containing
// the node features, addresses and extra signed fields.
func batchLoadNodeData(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, nodeIDs []int64) (*batchNodeData, error) {

	// Batch load the node features.
	features, err := batchLoadNodeFeaturesHelper(ctx, cfg, db, nodeIDs)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load node "+
			"features: %w", err)
	}

	// Batch load the node addresses.
	addrs, err := batchLoadNodeAddressesHelper(ctx, cfg, db, nodeIDs)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load node "+
			"addresses: %w", err)
	}

	// Batch load the node extra signed fields.
	extraTypes, err := batchLoadNodeExtraTypesHelper(ctx, cfg, db, nodeIDs)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load node extra "+
			"signed fields: %w", err)
	}

	return &batchNodeData{
		features:    features,
		addresses:   addrs,
		extraFields: extraTypes,
	}, nil
}

// batchLoadNodeFeaturesHelper loads node features for a batch of node IDs
// using ExecuteBatchQuery wrapper around the GetNodeFeaturesBatch query.
func batchLoadNodeFeaturesHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	nodeIDs []int64) (map[int64][]int, error) {

	features := make(map[int64][]int)

	return features, sqldb.ExecuteBatchQuery(
		ctx, cfg, nodeIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context, ids []int64) ([]sqlc.GraphNodeFeature,
			error) {

			return db.GetNodeFeaturesBatch(ctx, ids)
		},
		func(ctx context.Context, feature sqlc.GraphNodeFeature) error {
			features[feature.NodeID] = append(
				features[feature.NodeID],
				int(feature.FeatureBit),
			)

			return nil
		},
	)
}

// batchLoadNodeAddressesHelper loads node addresses using ExecuteBatchQuery
// wrapper around the GetNodeAddressesBatch query. It returns a map from
// node ID to a slice of nodeAddress structs.
func batchLoadNodeAddressesHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	nodeIDs []int64) (map[int64][]nodeAddress, error) {

	addrs := make(map[int64][]nodeAddress)

	return addrs, sqldb.ExecuteBatchQuery(
		ctx, cfg, nodeIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context, ids []int64) ([]sqlc.GraphNodeAddress,
			error) {

			return db.GetNodeAddressesBatch(ctx, ids)
		},
		func(ctx context.Context, addr sqlc.GraphNodeAddress) error {
			addrs[addr.NodeID] = append(
				addrs[addr.NodeID], nodeAddress{
					addrType: dbAddressType(addr.Type),
					position: addr.Position,
					address:  addr.Address,
				},
			)

			return nil
		},
	)
}

// batchLoadNodeExtraTypesHelper loads node extra type bytes for a batch of
// node IDs using ExecuteBatchQuery wrapper around the GetNodeExtraTypesBatch
// query.
func batchLoadNodeExtraTypesHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	nodeIDs []int64) (map[int64]map[uint64][]byte, error) {

	extraFields := make(map[int64]map[uint64][]byte)

	callback := func(ctx context.Context,
		field sqlc.GraphNodeExtraType) error {

		if extraFields[field.NodeID] == nil {
			extraFields[field.NodeID] = make(map[uint64][]byte)
		}
		extraFields[field.NodeID][uint64(field.Type)] = field.Value

		return nil
	}

	return extraFields, sqldb.ExecuteBatchQuery(
		ctx, cfg, nodeIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context, ids []int64) (
			[]sqlc.GraphNodeExtraType, error) {

			return db.GetNodeExtraTypesBatch(ctx, ids)
		},
		callback,
	)
}

// buildChanPoliciesWithBatchData builds two models.ChannelEdgePolicy instances
// from the provided sqlc.GraphChannelPolicy records and the
// provided batchChannelData.
func buildChanPoliciesWithBatchData(dbPol1, dbPol2 *sqlc.GraphChannelPolicy,
	channelID uint64, node1, node2 route.Vertex,
	batchData *batchChannelData) (*models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	pol1, err := buildChanPolicyWithBatchData(
		dbPol1, channelID, node2, batchData,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build policy1: %w", err)
	}

	pol2, err := buildChanPolicyWithBatchData(
		dbPol2, channelID, node1, batchData,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build policy2: %w", err)
	}

	return pol1, pol2, nil
}

// buildChanPolicyWithBatchData builds a models.ChannelEdgePolicy instance from
// the provided sqlc.GraphChannelPolicy and the provided batchChannelData.
func buildChanPolicyWithBatchData(dbPol *sqlc.GraphChannelPolicy,
	channelID uint64, toNode route.Vertex,
	batchData *batchChannelData) (*models.ChannelEdgePolicy, error) {

	if dbPol == nil {
		return nil, nil
	}

	var dbPol1Extras map[uint64][]byte
	if extras, exists := batchData.policyExtras[dbPol.ID]; exists {
		dbPol1Extras = extras
	} else {
		dbPol1Extras = make(map[uint64][]byte)
	}

	return buildChanPolicy(*dbPol, channelID, dbPol1Extras, toNode)
}

// batchChannelData holds all the related data for a batch of channels.
type batchChannelData struct {
	// chanFeatures is a map from DB channel ID to a slice of feature bits.
	chanfeatures map[int64][]int

	// chanExtras is a map from DB channel ID to a map of TLV type to
	// extra signed field bytes.
	chanExtraTypes map[int64]map[uint64][]byte

	// policyExtras is a map from DB channel policy ID to a map of TLV type
	// to extra signed field bytes.
	policyExtras map[int64]map[uint64][]byte
}

// batchLoadChannelData loads all related data for batches of channels and
// policies.
func batchLoadChannelData(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, channelIDs []int64,
	policyIDs []int64) (*batchChannelData, error) {

	batchData := &batchChannelData{
		chanfeatures:   make(map[int64][]int),
		chanExtraTypes: make(map[int64]map[uint64][]byte),
		policyExtras:   make(map[int64]map[uint64][]byte),
	}

	// Batch load channel features and extras
	var err error
	if len(channelIDs) > 0 {
		batchData.chanfeatures, err = batchLoadChannelFeaturesHelper(
			ctx, cfg, db, channelIDs,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to batch load "+
				"channel features: %w", err)
		}

		batchData.chanExtraTypes, err = batchLoadChannelExtrasHelper(
			ctx, cfg, db, channelIDs,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to batch load "+
				"channel extras: %w", err)
		}
	}

	if len(policyIDs) > 0 {
		policyExtras, err := batchLoadChannelPolicyExtrasHelper(
			ctx, cfg, db, policyIDs,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to batch load "+
				"policy extras: %w", err)
		}
		batchData.policyExtras = policyExtras
	}

	return batchData, nil
}

// batchLoadChannelFeaturesHelper loads channel features for a batch of
// channel IDs using ExecuteBatchQuery wrapper around the
// GetChannelFeaturesBatch query. It returns a map from DB channel ID to a
// slice of feature bits.
func batchLoadChannelFeaturesHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	channelIDs []int64) (map[int64][]int, error) {

	features := make(map[int64][]int)

	return features, sqldb.ExecuteBatchQuery(
		ctx, cfg, channelIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context,
			ids []int64) ([]sqlc.GraphChannelFeature, error) {

			return db.GetChannelFeaturesBatch(ctx, ids)
		},
		func(ctx context.Context,
			feature sqlc.GraphChannelFeature) error {

			features[feature.ChannelID] = append(
				features[feature.ChannelID],
				int(feature.FeatureBit),
			)

			return nil
		},
	)
}

// batchLoadChannelExtrasHelper loads channel extra types for a batch of
// channel IDs using ExecuteBatchQuery wrapper around the GetChannelExtrasBatch
// query. It returns a map from DB channel ID to a map of TLV type to extra
// signed field bytes.
func batchLoadChannelExtrasHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	channelIDs []int64) (map[int64]map[uint64][]byte, error) {

	extras := make(map[int64]map[uint64][]byte)

	cb := func(ctx context.Context,
		extra sqlc.GraphChannelExtraType) error {

		if extras[extra.ChannelID] == nil {
			extras[extra.ChannelID] = make(map[uint64][]byte)
		}
		extras[extra.ChannelID][uint64(extra.Type)] = extra.Value

		return nil
	}

	return extras, sqldb.ExecuteBatchQuery(
		ctx, cfg, channelIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context,
			ids []int64) ([]sqlc.GraphChannelExtraType, error) {

			return db.GetChannelExtrasBatch(ctx, ids)
		}, cb,
	)
}

// batchLoadChannelPolicyExtrasHelper loads channel policy extra types for a
// batch of policy IDs using ExecuteBatchQuery wrapper around the
// GetChannelPolicyExtraTypesBatch query. It returns a map from DB policy ID to
// a map of TLV type to extra signed field bytes.
func batchLoadChannelPolicyExtrasHelper(ctx context.Context,
	cfg *sqldb.QueryConfig, db SQLQueries,
	policyIDs []int64) (map[int64]map[uint64][]byte, error) {

	extras := make(map[int64]map[uint64][]byte)

	return extras, sqldb.ExecuteBatchQuery(
		ctx, cfg, policyIDs,
		func(id int64) int64 {
			return id
		},
		func(ctx context.Context, ids []int64) (
			[]sqlc.GetChannelPolicyExtraTypesBatchRow, error) {

			return db.GetChannelPolicyExtraTypesBatch(ctx, ids)
		},
		func(ctx context.Context,
			row sqlc.GetChannelPolicyExtraTypesBatchRow) error {

			if extras[row.PolicyID] == nil {
				extras[row.PolicyID] = make(map[uint64][]byte)
			}
			extras[row.PolicyID][uint64(row.Type)] = row.Value

			return nil
		},
	)
}

// forEachNodePaginated executes a paginated query to process each node in the
// graph. It uses the provided SQLQueries interface to fetch nodes in batches
// and applies the provided processNode function to each node.
func forEachNodePaginated(ctx context.Context, cfg *sqldb.QueryConfig,
	db SQLQueries, protocol lnwire.GossipVersion,
	processNode func(context.Context, int64,
		*models.Node) error) error {

	pageQueryFunc := func(ctx context.Context, lastID int64,
		limit int32) ([]sqlc.GraphNode, error) {

		return db.ListNodesPaginated(
			ctx, sqlc.ListNodesPaginatedParams{
				Version: int16(protocol),
				ID:      lastID,
				Limit:   limit,
			},
		)
	}

	extractPageCursor := func(node sqlc.GraphNode) int64 {
		return node.ID
	}

	collectFunc := func(node sqlc.GraphNode) (int64, error) {
		return node.ID, nil
	}

	batchQueryFunc := func(ctx context.Context,
		nodeIDs []int64) (*batchNodeData, error) {

		return batchLoadNodeData(ctx, cfg, db, nodeIDs)
	}

	processItem := func(ctx context.Context, dbNode sqlc.GraphNode,
		batchData *batchNodeData) error {

		node, err := buildNodeWithBatchData(dbNode, batchData)
		if err != nil {
			return fmt.Errorf("unable to build "+
				"node(id=%d): %w", dbNode.ID, err)
		}

		return processNode(ctx, dbNode.ID, node)
	}

	return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
		ctx, cfg, int64(-1), pageQueryFunc, extractPageCursor,
		collectFunc, batchQueryFunc, processItem,
	)
}

// forEachChannelWithPolicies executes a paginated query to process each channel
// with policies in the graph.
func forEachChannelWithPolicies(ctx context.Context, db SQLQueries,
	cfg *SQLStoreConfig, processChannel func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	type channelBatchIDs struct {
		channelID int64
		policyIDs []int64
	}

	pageQueryFunc := func(ctx context.Context, lastID int64,
		limit int32) ([]sqlc.ListChannelsWithPoliciesPaginatedRow,
		error) {

		return db.ListChannelsWithPoliciesPaginated(
			ctx, sqlc.ListChannelsWithPoliciesPaginatedParams{
				Version: int16(lnwire.GossipVersion1),
				ID:      lastID,
				Limit:   limit,
			},
		)
	}

	extractPageCursor := func(
		row sqlc.ListChannelsWithPoliciesPaginatedRow) int64 {

		return row.GraphChannel.ID
	}

	collectFunc := func(row sqlc.ListChannelsWithPoliciesPaginatedRow) (
		channelBatchIDs, error) {

		ids := channelBatchIDs{
			channelID: row.GraphChannel.ID,
		}

		// Extract policy IDs from the row.
		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return ids, err
		}

		if dbPol1 != nil {
			ids.policyIDs = append(ids.policyIDs, dbPol1.ID)
		}
		if dbPol2 != nil {
			ids.policyIDs = append(ids.policyIDs, dbPol2.ID)
		}

		return ids, nil
	}

	batchDataFunc := func(ctx context.Context,
		allIDs []channelBatchIDs) (*batchChannelData, error) {

		// Separate channel IDs from policy IDs.
		var (
			channelIDs = make([]int64, len(allIDs))
			policyIDs  = make([]int64, 0, len(allIDs)*2)
		)

		for i, ids := range allIDs {
			channelIDs[i] = ids.channelID
			policyIDs = append(policyIDs, ids.policyIDs...)
		}

		return batchLoadChannelData(
			ctx, cfg.QueryCfg, db, channelIDs, policyIDs,
		)
	}

	processItem := func(ctx context.Context,
		row sqlc.ListChannelsWithPoliciesPaginatedRow,
		batchData *batchChannelData) error {

		node1, node2, err := buildNodeVertices(
			row.Node1Pubkey, row.Node2Pubkey,
		)
		if err != nil {
			return err
		}

		edge, err := buildEdgeInfoWithBatchData(
			cfg.ChainHash, row.GraphChannel, node1, node2,
			batchData,
		)
		if err != nil {
			return fmt.Errorf("unable to build channel info: %w",
				err)
		}

		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return err
		}

		p1, p2, err := buildChanPoliciesWithBatchData(
			dbPol1, dbPol2, edge.ChannelID, node1, node2, batchData,
		)
		if err != nil {
			return err
		}

		return processChannel(edge, p1, p2)
	}

	return sqldb.ExecuteCollectAndBatchWithSharedDataQuery(
		ctx, cfg.QueryCfg, int64(-1), pageQueryFunc, extractPageCursor,
		collectFunc, batchDataFunc, processItem,
	)
}

// buildDirectedChannel builds a DirectedChannel instance from the provided
// data.
func buildDirectedChannel(chain chainhash.Hash, nodeID int64,
	nodePub route.Vertex, channelRow sqlc.ListChannelsForNodeIDsRow,
	channelBatchData *batchChannelData, features *lnwire.FeatureVector,
	toNodeCallback func() route.Vertex) (*DirectedChannel, error) {

	node1, node2, err := buildNodeVertices(
		channelRow.Node1Pubkey, channelRow.Node2Pubkey,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build node vertices: %w", err)
	}

	edge, err := buildEdgeInfoWithBatchData(
		chain, channelRow.GraphChannel, node1, node2, channelBatchData,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build channel info: %w", err)
	}

	dbPol1, dbPol2, err := extractChannelPolicies(channelRow)
	if err != nil {
		return nil, fmt.Errorf("unable to extract channel policies: %w",
			err)
	}

	p1, p2, err := buildChanPoliciesWithBatchData(
		dbPol1, dbPol2, edge.ChannelID, node1, node2,
		channelBatchData,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build channel policies: %w",
			err)
	}

	// Determine outgoing and incoming policy for this specific node.
	p1ToNode := channelRow.GraphChannel.NodeID2
	p2ToNode := channelRow.GraphChannel.NodeID1
	outPolicy, inPolicy := p1, p2
	if (p1 != nil && p1ToNode == nodeID) ||
		(p2 != nil && p2ToNode != nodeID) {

		outPolicy, inPolicy = p2, p1
	}

	// Build cached policy.
	var cachedInPolicy *models.CachedEdgePolicy
	if inPolicy != nil {
		cachedInPolicy = models.NewCachedPolicy(inPolicy)
		cachedInPolicy.ToNodePubKey = toNodeCallback
		cachedInPolicy.ToNodeFeatures = features
	}

	// Extract inbound fee.
	var inboundFee lnwire.Fee
	if outPolicy != nil {
		outPolicy.InboundFee.WhenSome(func(fee lnwire.Fee) {
			inboundFee = fee
		})
	}

	// Build directed channel.
	directedChannel := &DirectedChannel{
		ChannelID:    edge.ChannelID,
		IsNode1:      nodePub == edge.NodeKey1Bytes,
		OtherNode:    edge.NodeKey2Bytes,
		Capacity:     edge.Capacity,
		OutPolicySet: outPolicy != nil,
		InPolicy:     cachedInPolicy,
		InboundFee:   inboundFee,
	}

	if nodePub == edge.NodeKey2Bytes {
		directedChannel.OtherNode = edge.NodeKey1Bytes
	}

	return directedChannel, nil
}

// batchBuildChannelEdges builds a slice of ChannelEdge instances from the
// provided rows. It uses batch loading for channels, policies, and nodes.
func batchBuildChannelEdges[T sqlc.ChannelAndNodes](ctx context.Context,
	cfg *SQLStoreConfig, db SQLQueries, rows []T) ([]ChannelEdge, error) {

	var (
		channelIDs = make([]int64, len(rows))
		policyIDs  = make([]int64, 0, len(rows)*2)
		nodeIDs    = make([]int64, 0, len(rows)*2)

		// nodeIDSet is used to ensure we only collect unique node IDs.
		nodeIDSet = make(map[int64]bool)

		// edges will hold the final channel edges built from the rows.
		edges = make([]ChannelEdge, 0, len(rows))
	)

	// Collect all IDs needed for batch loading.
	for i, row := range rows {
		channelIDs[i] = row.Channel().ID

		// Collect policy IDs
		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return nil, fmt.Errorf("unable to extract channel "+
				"policies: %w", err)
		}
		if dbPol1 != nil {
			policyIDs = append(policyIDs, dbPol1.ID)
		}
		if dbPol2 != nil {
			policyIDs = append(policyIDs, dbPol2.ID)
		}

		var (
			node1ID = row.Node1().ID
			node2ID = row.Node2().ID
		)

		// Collect unique node IDs.
		if !nodeIDSet[node1ID] {
			nodeIDs = append(nodeIDs, node1ID)
			nodeIDSet[node1ID] = true
		}

		if !nodeIDSet[node2ID] {
			nodeIDs = append(nodeIDs, node2ID)
			nodeIDSet[node2ID] = true
		}
	}

	// Batch the data for all the channels and policies.
	channelBatchData, err := batchLoadChannelData(
		ctx, cfg.QueryCfg, db, channelIDs, policyIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load channel and "+
			"policy data: %w", err)
	}

	// Batch the data for all the nodes.
	nodeBatchData, err := batchLoadNodeData(ctx, cfg.QueryCfg, db, nodeIDs)
	if err != nil {
		return nil, fmt.Errorf("unable to batch load node data: %w",
			err)
	}

	// Build all channel edges using batch data.
	for _, row := range rows {
		// Build nodes using batch data.
		node1, err := buildNodeWithBatchData(row.Node1(), nodeBatchData)
		if err != nil {
			return nil, fmt.Errorf("unable to build node1: %w", err)
		}

		node2, err := buildNodeWithBatchData(row.Node2(), nodeBatchData)
		if err != nil {
			return nil, fmt.Errorf("unable to build node2: %w", err)
		}

		// Build channel info using batch data.
		channel, err := buildEdgeInfoWithBatchData(
			cfg.ChainHash, row.Channel(), node1.PubKeyBytes,
			node2.PubKeyBytes, channelBatchData,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to build channel "+
				"info: %w", err)
		}

		// Extract and build policies using batch data.
		dbPol1, dbPol2, err := extractChannelPolicies(row)
		if err != nil {
			return nil, fmt.Errorf("unable to extract channel "+
				"policies: %w", err)
		}

		p1, p2, err := buildChanPoliciesWithBatchData(
			dbPol1, dbPol2, channel.ChannelID,
			node1.PubKeyBytes, node2.PubKeyBytes, channelBatchData,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to build channel "+
				"policies: %w", err)
		}

		edges = append(edges, ChannelEdge{
			Info:    channel,
			Policy1: p1,
			Policy2: p2,
			Node1:   node1,
			Node2:   node2,
		})
	}

	return edges, nil
}

// batchBuildChannelInfo builds a slice of models.ChannelEdgeInfo
// instances from the provided rows using batch loading for channel data.
func batchBuildChannelInfo[T sqlc.ChannelAndNodeIDs](ctx context.Context,
	cfg *SQLStoreConfig, db SQLQueries, rows []T) (
	[]*models.ChannelEdgeInfo, []int64, error) {

	if len(rows) == 0 {
		return nil, nil, nil
	}

	// Collect all the channel IDs needed for batch loading.
	channelIDs := make([]int64, len(rows))
	for i, row := range rows {
		channelIDs[i] = row.Channel().ID
	}

	// Batch load the channel data.
	channelBatchData, err := batchLoadChannelData(
		ctx, cfg.QueryCfg, db, channelIDs, nil,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to batch load channel "+
			"data: %w", err)
	}

	// Build all channel edges using batch data.
	edges := make([]*models.ChannelEdgeInfo, 0, len(rows))
	for _, row := range rows {
		node1, node2, err := buildNodeVertices(
			row.Node1Pub(), row.Node2Pub(),
		)
		if err != nil {
			return nil, nil, err
		}

		// Build channel info using batch data
		info, err := buildEdgeInfoWithBatchData(
			cfg.ChainHash, row.Channel(), node1, node2,
			channelBatchData,
		)
		if err != nil {
			return nil, nil, err
		}

		edges = append(edges, info)
	}

	return edges, channelIDs, nil
}

// handleZombieMarking is a helper function that handles the logic of
// marking a channel as a zombie in the database. It takes into account whether
// we are in strict zombie pruning mode, and adjusts the node public keys
// accordingly based on the last update timestamps of the channel policies.
func handleZombieMarking(ctx context.Context, db SQLQueries,
	row sqlc.GetChannelsBySCIDWithPoliciesRow, info *models.ChannelEdgeInfo,
	strictZombiePruning bool, scid uint64) error {

	nodeKey1, nodeKey2 := info.NodeKey1Bytes, info.NodeKey2Bytes

	if strictZombiePruning {
		var e1UpdateTime, e2UpdateTime *time.Time
		if row.Policy1LastUpdate.Valid {
			e1Time := time.Unix(row.Policy1LastUpdate.Int64, 0)
			e1UpdateTime = &e1Time
		}
		if row.Policy2LastUpdate.Valid {
			e2Time := time.Unix(row.Policy2LastUpdate.Int64, 0)
			e2UpdateTime = &e2Time
		}

		nodeKey1, nodeKey2 = makeZombiePubkeys(
			info.NodeKey1Bytes, info.NodeKey2Bytes, e1UpdateTime,
			e2UpdateTime,
		)
	}

	return db.UpsertZombieChannel(
		ctx, sqlc.UpsertZombieChannelParams{
			Version:  int16(lnwire.GossipVersion1),
			Scid:     channelIDToBytes(scid),
			NodeKey1: nodeKey1[:],
			NodeKey2: nodeKey2[:],
		},
	)
}
