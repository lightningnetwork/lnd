package migration1

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/graph/db/migration1/models"
	"github.com/lightningnetwork/lnd/graph/db/migration1/sqlc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"github.com/lightningnetwork/lnd/sqldb"
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
	GetNodeByPubKey(ctx context.Context, arg sqlc.GetNodeByPubKeyParams) (sqlc.GraphNode, error)
	GetNodesByIDs(ctx context.Context, ids []int64) ([]sqlc.GraphNode, error)
	GetNodeIDByPubKey(ctx context.Context, arg sqlc.GetNodeIDByPubKeyParams) (int64, error)
	GetNodesByLastUpdateRange(ctx context.Context, arg sqlc.GetNodesByLastUpdateRangeParams) ([]sqlc.GraphNode, error)
	ListNodesPaginated(ctx context.Context, arg sqlc.ListNodesPaginatedParams) ([]sqlc.GraphNode, error)
	ListNodeIDsAndPubKeys(ctx context.Context, arg sqlc.ListNodeIDsAndPubKeysParams) ([]sqlc.ListNodeIDsAndPubKeysRow, error)
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
func NewSQLStore(cfg *SQLStoreConfig, db BatchedSQLQueries) (*SQLStore, error) {
	s := &SQLStore{
		cfg: cfg,
		db:  db,
	}

	return s, nil
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

// sourceNode returns the DB node ID and pub key of the source node for the
// specified protocol version.
func (s *SQLStore) getSourceNode(ctx context.Context, db SQLQueries,
	version lnwire.GossipVersion) (int64, route.Vertex, error) {

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
