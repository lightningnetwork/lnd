package sources

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"image/color"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon.v2"
)

// DefaultRPCTimeout is the default timeout for RPC calls to the remote graph
// source.
const DefaultRPCTimeout = 5 * time.Second

// Compile-time check that RPCClient implements RemoteGraph.
var _ RemoteGraph = (*RPCClient)(nil)

// RPCConfig holds the configuration options for the remote graph RPC client.
//
//nolint:ll
type RPCConfig struct {
	RPCHost      string        `long:"rpchost" description:"The remote graph's RPC host:port"`
	MacaroonPath string        `long:"macaroonpath" description:"The macaroon to use for authenticating with the remote graph source"`
	TLSCertPath  string        `long:"tlscertpath" description:"The TLS certificate to use for establishing the remote graph's identity"`
	Timeout      time.Duration `long:"timeout" description:"The timeout for connecting to the remote graph. Valid time units are {s, m, h}."`

	// ParseAddress is a function that parses a string address (like
	// "host:port") into a net.Addr. This is injected to avoid a
	// dependency on lncfg from this package.
	ParseAddress func(strAddress string) (net.Addr, error)
}

// TopologyCallbacks defines callbacks invoked when the remote graph's topology
// changes. These allow the caller (typically the server) to keep the local
// graph cache in sync with the remote graph.
type TopologyCallbacks struct {
	// OnChannel is called when a new channel is discovered or an existing
	// channel's edge info is received.
	OnChannel func(info *models.CachedEdgeInfo)

	// OnChannelUpdate is called when a channel policy update is received.
	OnChannelUpdate func(policy *models.CachedEdgePolicy,
		fromNode, toNode route.Vertex)

	// OnNodeUpdate is called when a node announcement is received.
	OnNodeUpdate func(node route.Vertex,
		features *lnwire.FeatureVector)

	// OnChannelClosed is called when a channel close is detected.
	OnChannelClosed func(chanID uint64)
}

// RPCClient implements the RemoteGraph interface by querying a remote LND node
// over gRPC.
type RPCClient struct {
	started atomic.Bool
	stopped atomic.Bool

	cfg *RPCConfig
	cb  *TopologyCallbacks

	grpcConn *grpc.ClientConn
	conn     lnrpc.LightningClient

	wg     sync.WaitGroup
	cancel fn.Option[context.CancelFunc]
}

// NewRPCClient constructs a new RPCClient.
func NewRPCClient(cfg *RPCConfig, cb *TopologyCallbacks) *RPCClient {
	return &RPCClient{
		cfg: cfg,
		cb:  cb,
	}
}

// rpcCtx returns a context with the configured RPC timeout applied. The
// returned cancel function must be called to release resources.
func (c *RPCClient) rpcCtx(
	ctx context.Context) (context.Context, context.CancelFunc) {

	return context.WithTimeout(ctx, c.cfg.Timeout)
}

// Start connects the client to the remote graph server, populates the graph
// cache from a full snapshot, and launches the topology subscription goroutine
// to keep the cache in sync with incremental updates.
func (c *RPCClient) Start() error {
	if !c.started.CompareAndSwap(false, true) {
		return nil
	}

	log.Info("Remote graph RPC client starting")

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = fn.Some(cancel)

	conn, err := connectRPC(
		ctx, c.cfg.RPCHost, c.cfg.TLSCertPath, c.cfg.MacaroonPath,
		c.cfg.Timeout,
	)
	if err != nil {
		return err
	}
	c.grpcConn = conn
	c.conn = lnrpc.NewLightningClient(conn)

	// TODO: Once #10065 (async graph cache population) lands, this
	// can be made async using the graphCacheState's applyUpdate
	// buffering so that startup doesn't block on the remote graph
	// fetch.
	if err := c.populateCache(ctx); err != nil {
		return err
	}

	if c.cb != nil {
		c.wg.Add(1)
		go c.handleNetworkUpdates(ctx)
	}

	return nil
}

// Stop cancels any goroutines and contexts created by the client.
func (c *RPCClient) Stop() error {
	if !c.stopped.CompareAndSwap(false, true) {
		return nil
	}

	log.Info("Remote graph RPC client stopping...")
	defer log.Info("Remote graph RPC client stopped")

	c.cancel.WhenSome(func(cancel context.CancelFunc) { cancel() })
	c.wg.Wait()

	if c.grpcConn != nil {
		if err := c.grpcConn.Close(); err != nil {
			log.Errorf("Error closing gRPC connection: %v", err)
		}
	}

	return nil
}

// populateCache performs an initial full fetch of the remote graph and feeds
// all nodes and channels into the registered topology callbacks. Once the
// cache is populated, it launches the topology subscription goroutine to keep
// the cache in sync with incremental updates.
func (c *RPCClient) populateCache(ctx context.Context) error {
	if c.cb == nil {
		return nil
	}

	log.Info("Populating graph cache from remote graph...")
	startTime := time.Now()

	graph, err := c.conn.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch remote graph: %w", err)
	}

	// First, add all node features.
	if c.cb.OnNodeUpdate != nil {
		for _, rpcNode := range graph.Nodes {
			pub, err := route.NewVertexFromStr(rpcNode.PubKey)
			if err != nil {
				return err
			}

			c.cb.OnNodeUpdate(
				pub, unmarshalFeatures(rpcNode.Features),
			)
		}
	}

	// Then add all channels and their policies.
	for _, edge := range graph.Edges {
		node1, err := route.NewVertexFromStr(edge.Node1Pub)
		if err != nil {
			return err
		}
		node2, err := route.NewVertexFromStr(edge.Node2Pub)
		if err != nil {
			return err
		}

		if c.cb.OnChannel != nil {
			c.cb.OnChannel(&models.CachedEdgeInfo{
				ChannelID:     edge.ChannelId,
				Capacity:      btcutil.Amount(edge.Capacity),
				NodeKey1Bytes: node1,
				NodeKey2Bytes: node2,
			})
		}

		if c.cb.OnChannelUpdate != nil {
			if edge.Node1Policy != nil {
				policy := makeCachedPolicy(
					edge.ChannelId,
					edge.Node1Policy,
					node2, true,
				)
				c.cb.OnChannelUpdate(
					policy, node1, node2,
				)
			}
			if edge.Node2Policy != nil {
				policy := makeCachedPolicy(
					edge.ChannelId,
					edge.Node2Policy,
					node1, false,
				)
				c.cb.OnChannelUpdate(
					policy, node2, node1,
				)
			}
		}
	}

	log.Infof("Remote graph cache populated with %d nodes and %d "+
		"channels in %v", len(graph.Nodes), len(graph.Edges),
		time.Since(startTime))

	return nil
}

// handleNetworkUpdates subscribes to topology updates from the remote LND
// node and forwards them to the registered callbacks. If the subscription
// drops, it reconnects with exponential backoff.
//
// NOTE: this MUST be run as a goroutine.
func (c *RPCClient) handleNetworkUpdates(ctx context.Context) {
	defer c.wg.Done()

	backoff := time.Second
	const maxBackoff = time.Minute

	for {
		err := c.subscribeAndProcess(ctx)

		// If the context was cancelled, we're shutting down.
		select {
		case <-ctx.Done():
			return
		default:
		}

		log.Warnf("Remote graph subscription error (retrying in "+
			"%v): %v", backoff, err)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// subscribeAndProcess opens a topology subscription and processes updates
// until an error occurs or the context is cancelled.
func (c *RPCClient) subscribeAndProcess(
	ctx context.Context) error {

	client, err := c.conn.SubscribeChannelGraph(
		ctx, &lnrpc.GraphTopologySubscription{},
	)
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	log.Info("Subscribed to remote graph topology updates")

	for {
		updates, err := client.Recv()
		if err != nil {
			return fmt.Errorf("recv error: %w", err)
		}

		for _, update := range updates.ChannelUpdates {
			if err := c.handleChannelUpdate(update); err != nil {
				log.Errorf("Error handling channel update: %v",
					err)
			}
		}

		for _, update := range updates.NodeUpdates {
			if err := c.handleNodeUpdate(update); err != nil {
				log.Errorf("Error handling node update: %v",
					err)
			}
		}

		for _, update := range updates.ClosedChans {
			if c.cb.OnChannelClosed != nil {
				c.cb.OnChannelClosed(update.ChanId)
			}
		}
	}
}

// handleNodeUpdate converts an lnrpc.NodeUpdate and invokes the OnNodeUpdate
// callback.
func (c *RPCClient) handleNodeUpdate(update *lnrpc.NodeUpdate) error {
	if c.cb.OnNodeUpdate == nil {
		return nil
	}

	pub, err := route.NewVertexFromStr(update.IdentityKey)
	if err != nil {
		return err
	}

	c.cb.OnNodeUpdate(pub, unmarshalFeatures(update.Features))

	return nil
}

// handleChannelUpdate converts an lnrpc.ChannelEdgeUpdate and invokes the
// OnChannel and OnChannelUpdate callbacks.
func (c *RPCClient) handleChannelUpdate(
	update *lnrpc.ChannelEdgeUpdate) error {

	fromNode, err := route.NewVertexFromStr(update.AdvertisingNode)
	if err != nil {
		return err
	}

	toNode, err := route.NewVertexFromStr(update.ConnectingNode)
	if err != nil {
		return err
	}

	// Lexicographically sort the pubkeys to determine node1/node2.
	var node1, node2 route.Vertex
	if bytes.Compare(fromNode[:], toNode[:]) == -1 {
		node1 = fromNode
		node2 = toNode
	} else {
		node1 = toNode
		node2 = fromNode
	}

	if c.cb.OnChannel != nil {
		edge := &models.CachedEdgeInfo{
			ChannelID:     update.ChanId,
			Capacity:      btcutil.Amount(update.Capacity),
			NodeKey1Bytes: node1,
			NodeKey2Bytes: node2,
		}
		c.cb.OnChannel(edge)
	}

	if update.RoutingPolicy != nil && c.cb.OnChannelUpdate != nil {
		policy := makeCachedPolicy(
			update.ChanId, update.RoutingPolicy, toNode,
			fromNode == node1,
		)
		c.cb.OnChannelUpdate(policy, fromNode, toNode)
	}

	return nil
}

// makeCachedPolicy converts an lnrpc.RoutingPolicy to a CachedEdgePolicy
// suitable for use with the graph cache.
func makeCachedPolicy(chanID uint64, rpcPolicy *lnrpc.RoutingPolicy,
	toNode [33]byte, isNode1 bool) *models.CachedEdgePolicy {

	policy := &models.CachedEdgePolicy{
		ChannelID:     chanID,
		IsNode1:       isNode1,
		IsDisabled:    rpcPolicy.Disabled,
		HasMaxHTLC:    rpcPolicy.MaxHtlcMsat > 0,
		TimeLockDelta: uint16(rpcPolicy.TimeLockDelta),
		MinHTLC:       lnwire.MilliSatoshi(rpcPolicy.MinHtlc),
		MaxHTLC:       lnwire.MilliSatoshi(rpcPolicy.MaxHtlcMsat),
		FeeBaseMSat:   lnwire.MilliSatoshi(rpcPolicy.FeeBaseMsat),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			rpcPolicy.FeeRateMilliMsat,
		),
		ToNodePubKey: func() route.Vertex {
			return toNode
		},
	}

	if rpcPolicy.InboundFeeRateMilliMsat != 0 ||
		rpcPolicy.InboundFeeBaseMsat != 0 {

		policy.InboundFee = fn.Some(lnwire.Fee{
			BaseFee: rpcPolicy.InboundFeeBaseMsat,
			FeeRate: rpcPolicy.InboundFeeRateMilliMsat,
		})
	}

	return policy
}

// ForEachNode iterates over all nodes from the remote graph.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) ForEachNode(ctx context.Context,
	cb func(*models.Node) error, reset func()) error {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	graph, err := c.conn.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	})
	if err != nil {
		return err
	}

	for _, rpcNode := range graph.Nodes {
		node, err := c.unmarshalNode(rpcNode)
		if err != nil {
			return err
		}

		if err := cb(node); err != nil {
			return err
		}
	}

	return nil
}

// ForEachChannel iterates over all channels from the remote graph.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy) error,
	reset func()) error {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	graph, err := c.conn.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
		IncludeAuthProof:   true,
	})
	if err != nil {
		return err
	}

	for _, edge := range graph.Edges {
		edgeInfo, p1, p2, err := unmarshalChannelEdge(edge)
		if err != nil {
			return err
		}

		if err := cb(edgeInfo, p1, p2); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNodeChannel iterates over all channels of the given node from the
// remote graph.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy) error,
	reset func()) error {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	info, err := c.conn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(nodePub[:]),
		IncludeChannels: true,
	})
	if isNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}

	for _, channel := range info.Channels {
		edge, p1, p2, err := unmarshalChannelEdge(channel)
		if err != nil {
			return err
		}

		if err := cb(edge, p1, p2); err != nil {
			return err
		}
	}

	return nil
}

// FetchChannelEdgesByID looks up a channel edge by channel ID.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) FetchChannelEdgesByID(ctx context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	info, err := c.conn.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: chanID,
	})
	if isNotFound(err) {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}

	return unmarshalChannelEdge(info)
}

// FetchChannelEdgesByOutpoint looks up a channel edge by funding outpoint.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) FetchChannelEdgesByOutpoint(ctx context.Context,
	op *wire.OutPoint) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	info, err := c.conn.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanPoint: op.String(),
	})
	if isNotFound(err) {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}

	return unmarshalChannelEdge(info)
}

// FetchNode looks up a node by its public key.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) FetchNode(ctx context.Context,
	nodePub route.Vertex) (*models.Node, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	resp, err := c.conn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(nodePub[:]),
		IncludeChannels: false,
	})
	if isNotFound(err) {
		return nil, graphdb.ErrGraphNodeNotFound
	} else if err != nil {
		return nil, err
	}

	return c.unmarshalNode(resp.Node)
}

// IsPublicNode checks whether the given node is known to the remote graph.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) IsPublicNode(ctx context.Context,
	pubKey [33]byte) (bool, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	_, err := c.conn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(pubKey[:]),
		IncludeChannels: false,
	})
	if isNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	return true, nil
}

// LookupAlias returns the alias for the given node.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) LookupAlias(ctx context.Context,
	pub *btcec.PublicKey) (string, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	pubBytes := route.NewVertex(pub)
	resp, err := c.conn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(pubBytes[:]),
		IncludeChannels: false,
	})
	if isNotFound(err) {
		return "", graphdb.ErrGraphNodeNotFound
	} else if err != nil {
		return "", err
	}

	if resp.Node == nil {
		return "", graphdb.ErrGraphNodeNotFound
	}

	return resp.Node.Alias, nil
}

// AddrsForNode returns the addresses for the given node.
//
// NOTE: this is part of the RemoteGraph interface.
func (c *RPCClient) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	ctx, cancel := c.rpcCtx(ctx)
	defer cancel()

	pubBytes := route.NewVertex(nodePub)
	resp, err := c.conn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(pubBytes[:]),
		IncludeChannels: false,
	})
	if isNotFound(err) {
		return false, nil, nil
	} else if err != nil {
		return false, nil, err
	}

	if resp.Node == nil {
		return false, nil, nil
	}

	addrs, err := c.unmarshalAddrs(resp.Node.Addresses)
	if err != nil {
		return false, nil, err
	}

	return true, addrs, nil
}

// connectRPC establishes a gRPC connection to the remote LND node.
func connectRPC(ctx context.Context, hostPort, tlsCertPath, macaroonPath string,
	timeout time.Duration) (*grpc.ClientConn, error) {

	certBytes, err := os.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("error reading TLS cert file %v: %w",
			tlsCertPath, err)
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM(certBytes) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}

	macBytes, err := os.ReadFile(macaroonPath)
	if err != nil {
		return nil, fmt.Errorf("error reading macaroon file %v: %w",
			macaroonPath, err)
	}
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("error decoding macaroon: %w", err)
	}

	macCred, err := macaroons.NewMacaroonCredential(mac)
	if err != nil {
		return nil, fmt.Errorf("error creating creds: %w", err)
	}

	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(
			cp, "",
		)),
		grpc.WithPerRPCCredentials(macCred),
		grpc.WithBlock(),
	}

	ctxt, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	conn, err := grpc.DialContext(ctxt, hostPort, opts...)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to RPC server: %w",
			err)
	}

	return conn, nil
}

// unmarshalNode converts an lnrpc.LightningNode to a models.Node.
func (c *RPCClient) unmarshalNode(node *lnrpc.LightningNode) (
	*models.Node, error) {

	if node == nil {
		return nil, fmt.Errorf("nil node received from remote")
	}

	pubKey, err := hex.DecodeString(node.PubKey)
	if err != nil {
		return nil, err
	}

	var pubKeyBytes [33]byte
	copy(pubKeyBytes[:], pubKey)

	extra, err := lnwire.CustomRecords(node.CustomRecords).Serialize()
	if err != nil {
		return nil, err
	}

	addrs, err := c.unmarshalAddrs(node.Addresses)
	if err != nil {
		return nil, err
	}

	nodeColor, err := parseColor(node.Color)
	if err != nil {
		return nil, err
	}

	features := unmarshalFeatures(node.Features)

	// Build the node model. We treat nodes from the remote as v1 since
	// the RPC doesn't currently convey gossip version info.
	result := &models.Node{
		Version:         lnwire.GossipVersion1,
		PubKeyBytes:     pubKeyBytes,
		LastUpdate:      time.Unix(int64(node.LastUpdate), 0),
		Addresses:       addrs,
		Alias:           fn.Some(node.Alias),
		Color:           fn.Some(nodeColor),
		Features:        features,
		ExtraOpaqueData: extra,
	}

	return result, nil
}

// unmarshalAddrs converts lnrpc.NodeAddress slice to net.Addr slice.
func (c *RPCClient) unmarshalAddrs(addrs []*lnrpc.NodeAddress) ([]net.Addr,
	error) {

	if c.cfg.ParseAddress == nil {
		return nil, fmt.Errorf("no address parser configured")
	}

	netAddrs := make([]net.Addr, 0, len(addrs))
	for _, addr := range addrs {
		netAddr, err := c.cfg.ParseAddress(addr.Addr)
		if err != nil {
			return nil, err
		}
		netAddrs = append(netAddrs, netAddr)
	}

	return netAddrs, nil
}

// unmarshalChannelEdge converts an lnrpc.ChannelEdge to internal model types.
func unmarshalChannelEdge(info *lnrpc.ChannelEdge) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	chanPoint, err := wire.NewOutPointFromString(info.ChanPoint)
	if err != nil {
		return nil, nil, nil, err
	}

	var (
		node1Bytes [33]byte
		node2Bytes [33]byte
	)
	node1, err := hex.DecodeString(info.Node1Pub)
	if err != nil {
		return nil, nil, nil, err
	}
	copy(node1Bytes[:], node1)

	node2, err := hex.DecodeString(info.Node2Pub)
	if err != nil {
		return nil, nil, nil, err
	}
	copy(node2Bytes[:], node2)

	extra, err := lnwire.CustomRecords(info.CustomRecords).Serialize()
	if err != nil {
		return nil, nil, nil, err
	}

	// Build the edge. We treat edges from the remote as v1 since the RPC
	// doesn't currently convey gossip version info.
	edge := &models.ChannelEdgeInfo{
		Version:         lnwire.GossipVersion1,
		ChannelID:       info.ChannelId,
		ChannelPoint:    *chanPoint,
		NodeKey1Bytes:   node1Bytes,
		NodeKey2Bytes:   node2Bytes,
		Capacity:        btcutil.Amount(info.Capacity),
		Features:        lnwire.EmptyFeatureVector(),
		ExtraOpaqueData: extra,
	}

	if info.AuthProof != nil {
		edge.AuthProof = models.NewV1ChannelAuthProof(
			info.AuthProof.NodeSig1,
			info.AuthProof.NodeSig2,
			info.AuthProof.BitcoinSig1,
			info.AuthProof.BitcoinSig2,
		)
	}

	var (
		policy1 *models.ChannelEdgePolicy
		policy2 *models.ChannelEdgePolicy
	)
	if info.Node1Policy != nil {
		policy1, err = unmarshalPolicy(
			info.ChannelId, info.Node1Policy, true, info.Node2Pub,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if info.Node2Policy != nil {
		policy2, err = unmarshalPolicy(
			info.ChannelId, info.Node2Policy, false, info.Node1Pub,
		)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return edge, policy1, policy2, nil
}

// unmarshalPolicy converts an lnrpc.RoutingPolicy to a
// models.ChannelEdgePolicy.
func unmarshalPolicy(channelID uint64, rpcPolicy *lnrpc.RoutingPolicy,
	node1 bool, toNodeStr string) (*models.ChannelEdgePolicy, error) {

	var chanFlags lnwire.ChanUpdateChanFlags
	if !node1 {
		chanFlags |= lnwire.ChanUpdateDirection
	}
	if rpcPolicy.Disabled {
		chanFlags |= lnwire.ChanUpdateDisabled
	}

	var msgFlags lnwire.ChanUpdateMsgFlags
	if rpcPolicy.MaxHtlcMsat > 0 {
		msgFlags |= lnwire.ChanUpdateRequiredMaxHtlc
	}

	extra, err := lnwire.CustomRecords(rpcPolicy.CustomRecords).Serialize()
	if err != nil {
		return nil, err
	}

	toNodeB, err := hex.DecodeString(toNodeStr)
	if err != nil {
		return nil, err
	}
	toNode, err := route.NewVertexFromBytes(toNodeB)
	if err != nil {
		return nil, err
	}

	policy := &models.ChannelEdgePolicy{
		Version:       lnwire.GossipVersion1,
		ChannelID:     channelID,
		LastUpdate:    time.Unix(int64(rpcPolicy.LastUpdate), 0),
		MessageFlags:  msgFlags,
		ChannelFlags:  chanFlags,
		TimeLockDelta: uint16(rpcPolicy.TimeLockDelta),
		MinHTLC:       lnwire.MilliSatoshi(rpcPolicy.MinHtlc),
		MaxHTLC:       lnwire.MilliSatoshi(rpcPolicy.MaxHtlcMsat),
		FeeBaseMSat:   lnwire.MilliSatoshi(rpcPolicy.FeeBaseMsat),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			rpcPolicy.FeeRateMilliMsat,
		),
		ToNode:          toNode,
		ExtraOpaqueData: extra,
	}

	if rpcPolicy.InboundFeeRateMilliMsat != 0 ||
		rpcPolicy.InboundFeeBaseMsat != 0 {

		policy.InboundFee = fn.Some(lnwire.Fee{
			BaseFee: rpcPolicy.InboundFeeBaseMsat,
			FeeRate: rpcPolicy.InboundFeeRateMilliMsat,
		})
	}

	return policy, nil
}

// unmarshalFeatures converts an lnrpc feature map to a lnwire.FeatureVector.
func unmarshalFeatures(
	features map[uint32]*lnrpc.Feature) *lnwire.FeatureVector {

	featureBits := make([]lnwire.FeatureBit, 0, len(features))
	featureNames := make(map[lnwire.FeatureBit]string)
	for bit, feature := range features {
		featureBits = append(featureBits, lnwire.FeatureBit(bit))
		featureNames[lnwire.FeatureBit(bit)] = feature.Name
	}

	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(featureBits...), featureNames,
	)
}

// parseColor parses a hex color string (e.g. "#3399ff") into a color.RGBA.
func parseColor(hexColor string) (color.RGBA, error) {
	if len(hexColor) == 0 {
		return color.RGBA{}, nil
	}

	// Strip leading '#' if present.
	if hexColor[0] == '#' {
		hexColor = hexColor[1:]
	}

	if len(hexColor) != 6 {
		return color.RGBA{}, fmt.Errorf("invalid color: %s", hexColor)
	}

	colorBytes, err := hex.DecodeString(hexColor)
	if err != nil {
		return color.RGBA{}, err
	}

	return color.RGBA{
		R: colorBytes[0],
		G: colorBytes[1],
		B: colorBytes[2],
	}, nil
}

// isNotFound returns true if the error is a gRPC NotFound status error.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}

	st, ok := status.FromError(err)
	if !ok {
		return false
	}

	return st.Code() == codes.NotFound
}
