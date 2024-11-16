package graphrpc

import (
	"context"
	"encoding/hex"
	"image/color"
	"net"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/discovery"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/graph/session"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Client is a wrapper that implements the sources.GraphSource interface using a
// grpc connection which it uses to communicate with a grpc GraphClient client
// along with an lnrpc.LightningClient.
type Client struct {
	// graphConn is a grpc client that implements the GraphClient service.
	graphConn GraphClient

	// lnConn is a grpc client that implements the LightningClient service.
	lnConn lnrpc.LightningClient

	resolveTCPAddr func(network, address string) (*net.TCPAddr, error)
}

// NewRemoteClient constructs a new Client that uses the given grpc connections
// to implement the sources.GraphSource interface.
func NewRemoteClient(conn *grpc.ClientConn,
	resolveTCPAddr func(network, address string) (*net.TCPAddr,
		error)) *Client {

	return &Client{
		graphConn:      NewGraphClient(conn),
		lnConn:         lnrpc.NewLightningClient(conn),
		resolveTCPAddr: resolveTCPAddr,
	}
}

// BetweennessCentrality queries the remote graph client for the normalised and
// non-normalised betweenness centrality for each node in the graph.
//
// NOTE: This is a part of the sources.GraphSource interface.
func (r *Client) BetweennessCentrality(ctx context.Context) (
	map[autopilot.NodeID]*models.BetweennessCentrality, error) {

	resp, err := r.graphConn.BetweennessCentrality(
		ctx, &BetweennessCentralityReq{},
	)
	if err != nil {
		return nil, err
	}

	centrality := make(map[autopilot.NodeID]*models.BetweennessCentrality)
	for _, node := range resp.NodeBetweenness {
		var id autopilot.NodeID
		copy(id[:], node.Node)
		centrality[id] = &models.BetweennessCentrality{
			Normalized:    node.Normalized,
			NonNormalized: node.NonNormalized,
		}
	}

	return centrality, nil
}

// GraphBootstrapper returns a NetworkPeerBootstrapper instance backed by a
// remote graph source. It can be queried for a set of random node addresses
// that can be used to establish new connections.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) GraphBootstrapper(ctx context.Context) (
	discovery.NetworkPeerBootstrapper, error) {

	resp, err := r.graphConn.BootstrapperName(ctx, &BoostrapperNameReq{})
	if err != nil {
		return nil, err
	}

	return &bootstrapper{
		name:   resp.Name,
		Client: r,
	}, nil
}

// NetworkStats queries the remote lnd node for statistics concerning the
// current state of the known channel graph within the network.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) NetworkStats(ctx context.Context) (*models.NetworkStats,
	error) {

	info, err := r.lnConn.GetNetworkInfo(ctx, &lnrpc.NetworkInfoRequest{})
	if err != nil {
		return nil, err
	}

	return &models.NetworkStats{
		Diameter:             info.GraphDiameter,
		MaxChanOut:           info.MaxOutDegree,
		NumNodes:             info.NumNodes,
		NumChannels:          info.NumChannels,
		TotalNetworkCapacity: btcutil.Amount(info.TotalNetworkCapacity),
		MinChanSize:          btcutil.Amount(info.MinChannelSize),
		MaxChanSize:          btcutil.Amount(info.MaxChannelSize),
		MedianChanSize:       btcutil.Amount(info.MedianChannelSizeSat),
		NumZombies:           info.NumZombieChans,
	}, nil
}

// NewPathFindTx returns a new read transaction that can be used for a single
// path finding session.
//
// NOTE: this currently returns a dummy transaction.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) NewPathFindTx(_ context.Context) (session.RTx, error) {
	return newRPCRTx(), nil
}

// ForEachNodeDirectedChannel queries the remote node for info regarding the
// node in question. It also obtains a list of all the nodes known channels and
// applies the call back to each channel. If the callback returns an error, then
// the iteration is halted with the error propagated back up to the caller.
// Unknown policies are passed into the callback as nil values.  No error is
// returned if the node is not found.
//
// NOTE: The read transaction passed in is ignored.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) ForEachNodeDirectedChannel(ctx context.Context, _ session.RTx,
	node route.Vertex,
	cb func(channel *graphdb.DirectedChannel) error) error {

	// Obtain the node info from the remote node and ask it to include
	// channel information.
	info, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(node[:]),
		IncludeChannels: true,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return nil
	} else if err != nil {
		return err
	}

	toNodeCallback := func() route.Vertex {
		return node
	}
	toNodeFeatures := unmarshalFeatures(info.Node.Features)

	// Convert each channel to the type expected by the call-back and then
	// apply the call-back.
	for _, channel := range info.Channels {
		e, p1, p2, err := unmarshalChannelInfo(channel)
		if err != nil {
			return err
		}

		var cachedInPolicy *models.CachedEdgePolicy
		if p2 != nil {
			cachedInPolicy = models.NewCachedPolicy(p2)
			cachedInPolicy.ToNodePubKey = toNodeCallback
			cachedInPolicy.ToNodeFeatures = toNodeFeatures
		}

		var inboundFee lnwire.Fee
		if p1 != nil {
			// Extract inbound fee. If there is a decoding error,
			// skip this edge.
			_, err := p1.ExtraOpaqueData.ExtractRecords(&inboundFee)
			if err != nil {
				return nil
			}
		}

		directedChannel := &graphdb.DirectedChannel{
			ChannelID:    e.ChannelID,
			IsNode1:      node == e.NodeKey1Bytes,
			OtherNode:    e.NodeKey2Bytes,
			Capacity:     e.Capacity,
			OutPolicySet: p1 != nil,
			InPolicy:     cachedInPolicy,
			InboundFee:   inboundFee,
		}

		if node == e.NodeKey2Bytes {
			directedChannel.OtherNode = e.NodeKey1Bytes
		}

		if err := cb(directedChannel); err != nil {
			return err
		}
	}

	return nil
}

// FetchNodeFeatures queries the remote node for the feature vector of the node
// in question. If no features are known for the node or the node itself is
// unknown, an empty feature vector is returned.
//
// NOTE: The read transaction passed in is ignored.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) FetchNodeFeatures(ctx context.Context, _ session.RTx,
	node route.Vertex) (*lnwire.FeatureVector, error) {

	// Query the remote node for information about the node in question.
	// There is no need to include channel information since we only care
	// about features.
	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(node[:]),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return lnwire.EmptyFeatureVector(), nil
	} else if err != nil {
		return nil, err
	}

	return unmarshalFeatures(resp.Node.Features), nil
}

// ForEachNode calls DescribeGraph on the remote node and applies the call back
// to each node in the response. No error is returned if no nodes are found.
//
// NOTE: this is an expensive call as it fetches all nodes from the network and
// so it is recommended that caching be used by both the client and the server
// where appropriate to reduce the number of calls to this method.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) ForEachNode(ctx context.Context,
	cb func(*models.LightningNode) error) error {

	graph, err := r.lnConn.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	})
	if err != nil {
		return err
	}

	// Iterate through all the nodes, convert the types to that expected
	// by the call-back and then apply the call-back.
	for _, node := range graph.Nodes {
		pubKey, err := hex.DecodeString(node.PubKey)
		if err != nil {
			return err
		}
		var pubKeyBytes [33]byte
		copy(pubKeyBytes[:], pubKey)

		extra, err := lnwire.CustomRecords(
			node.CustomRecords,
		).Serialize()
		if err != nil {
			return err
		}

		addrs, err := r.unmarshalAddrs(node.Addresses)
		if err != nil {
			return err
		}

		// If we have addresses for this node, or the node has an alias,
		// or the node has extra data or a non-empty feature vector,
		// then we assume the remote node has a node announcement for
		// this node.
		haveNodeAnnouncement := len(addrs) > 0 || node.Alias != "" ||
			len(extra) > 0 || len(node.Features) > 0

		n := &models.LightningNode{
			PubKeyBytes:          pubKeyBytes,
			HaveNodeAnnouncement: haveNodeAnnouncement,
			LastUpdate: time.Unix(
				int64(node.LastUpdate), 0,
			),
			Addresses:       addrs,
			Color:           color.RGBA{},
			Alias:           node.Alias,
			Features:        unmarshalFeatures(node.Features),
			ExtraOpaqueData: extra,
		}

		err = cb(n)
		if err != nil {
			return err
		}
	}

	return nil
}

// ForEachChannel calls DescribeGraph on the remote node to fetch all the
// channels known to it. It then invokes the passed callback for each edge.
// An edge's policy structs may be nil if the ChannelUpdate in question has not
// yet been received for the channel. No error is returned if no channels are
// found.
//
// NOTE: this is an expensive call as it fetches all nodes from the network and
// so it is recommended that caching be used by both the client and the server
// where appropriate to reduce the number of calls to this method.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) ForEachChannel(ctx context.Context,
	cb func(*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	graph, err := r.lnConn.DescribeGraph(ctx, &lnrpc.ChannelGraphRequest{
		IncludeUnannounced: true,
	})
	if err != nil {
		return err
	}

	for _, edge := range graph.Edges {
		edgeInfo, policy1, policy2, err := unmarshalChannelInfo(edge)
		if err != nil {
			return err
		}

		if err := cb(edgeInfo, policy1, policy2); err != nil {
			return err
		}
	}

	return nil
}

// ForEachNodeChannel fetches all the channels for the given node from the
// remote node and applies the call back to each channel and its policies.
// The first edge policy is the outgoing edge *to* the connecting node, while
// the second is the incoming edge *from* the connecting node. Unknown policies
// are passed into the callback as nil values. No error is returned if the node
// is not found.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) ForEachNodeChannel(ctx context.Context,
	nodePub route.Vertex, cb func(*models.ChannelEdgeInfo,
		*models.ChannelEdgePolicy,
		*models.ChannelEdgePolicy) error) error {

	info, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(nodePub[:]),
		IncludeChannels: true,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return nil
	} else if err != nil {
		return err
	}

	for _, channel := range info.Channels {
		edge, policy1, policy2, err := unmarshalChannelInfo(channel)
		if err != nil {
			return err
		}

		if err := cb(edge, policy1, policy2); err != nil {
			return err
		}
	}

	return nil
}

// FetchLightningNode queries the remote node for information regarding the node
// in question. If the node isn't found in the database, then
// graphdb.ErrGraphNodeNotFound is returned.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) FetchLightningNode(ctx context.Context,
	nodePub route.Vertex) (*models.LightningNode, error) {

	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(nodePub[:]),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return nil, graphdb.ErrGraphNodeNotFound
	} else if err != nil {
		return nil, err
	}

	node := resp.Node

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

	addrs, err := r.unmarshalAddrs(resp.Node.Addresses)
	if err != nil {
		return nil, err
	}

	return &models.LightningNode{
		PubKeyBytes:          pubKeyBytes,
		HaveNodeAnnouncement: resp.IsPublic,
		LastUpdate:           time.Unix(int64(node.LastUpdate), 0),
		Addresses:            addrs,
		Color:                color.RGBA{},
		Alias:                node.Alias,
		Features:             unmarshalFeatures(node.Features),
		ExtraOpaqueData:      extra,
	}, nil
}

// FetchChannelEdgesByOutpoint queries the remote node for the channel edge info
// and most recent channel edge policies for a given outpoint. If the channel
// can't be found, then graphdb.ErrEdgeNotFound is returned.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) FetchChannelEdgesByOutpoint(ctx context.Context,
	point *wire.OutPoint) (*models.ChannelEdgeInfo,
	*models.ChannelEdgePolicy, *models.ChannelEdgePolicy, error) {

	info, err := r.lnConn.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanPoint: point.String(),
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return nil, nil, nil, graphdb.ErrEdgeNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}

	return unmarshalChannelInfo(info)
}

// FetchChannelEdgesByID queries the remote node for the channel edge info and
// most recent channel edge policies for a given channel ID. If the channel
// can't be found, then graphdb.ErrEdgeNotFound is returned.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) FetchChannelEdgesByID(ctx context.Context,
	chanID uint64) (*models.ChannelEdgeInfo, *models.ChannelEdgePolicy,
	*models.ChannelEdgePolicy, error) {

	info, err := r.lnConn.GetChanInfo(ctx, &lnrpc.ChanInfoRequest{
		ChanId: chanID,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	return unmarshalChannelInfo(info)
}

// IsPublicNode queries the remote node for information regarding the node in
// question and returns whether the node is seen as public by the remote node.
// If this node is unknown, then graphdb.ErrGraphNodeNotFound is returned.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) IsPublicNode(ctx context.Context, pubKey [33]byte) (bool,
	error) {

	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(pubKey[:]),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return false, graphdb.ErrGraphNodeNotFound
	} else if err != nil {
		return false, err
	}

	return resp.IsPublic, nil
}

// AddrsForNode queries the remote node for all the addresses it knows about for
// the node in question. The returned boolean indicates if the given node is
// unknown to the remote node.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) AddrsForNode(ctx context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey: hex.EncodeToString(
			nodePub.SerializeCompressed(),
		),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return false, nil, nil
	} else if err != nil {
		return false, nil, err
	}

	addrs, err := r.unmarshalAddrs(resp.Node.Addresses)
	if err != nil {
		return false, nil, err
	}

	return true, addrs, nil
}

// HasLightningNode determines if the graph has a vertex identified by the
// target node identity public key. If the node exists in the database, a
// timestamp of when the data for the node was lasted updated is returned along
// with a true boolean. Otherwise, an empty time.Time is returned with a false
// boolean and a nil error.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) HasLightningNode(ctx context.Context, nodePub [33]byte) (
	time.Time, bool, error) {

	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(nodePub[:]),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return time.Time{}, false, nil
	} else if err != nil {
		return time.Time{}, false, err
	}

	return time.Unix(int64(resp.Node.LastUpdate), 0), true, nil
}

// LookupAlias attempts to return the alias as advertised by the target node.
// graphdb.ErrNodeAliasNotFound is returned if the alias is not found.
//
// NOTE: this is part of the sources.GraphSource interface.
func (r *Client) LookupAlias(ctx context.Context, pub *btcec.PublicKey) (
	string, error) {

	resp, err := r.lnConn.GetNodeInfo(ctx, &lnrpc.NodeInfoRequest{
		PubKey:          hex.EncodeToString(pub.SerializeCompressed()),
		IncludeChannels: false,
	})
	if st, ok := status.FromError(err); ok && st.Code() == codes.NotFound {
		return "", graphdb.ErrNodeAliasNotFound
	} else if err != nil {
		return "", err
	}

	return resp.Node.Alias, nil
}

// rpcRTx implements the graphdb.RTx interface. It is currently not backed by
// any transaction as the graph source is remote.
type rpcRTx struct {
}

// newRPCRTx creates a new rpcRTx.
func newRPCRTx() *rpcRTx {
	return &rpcRTx{}
}

// Close closes the underlying transaction.
//
// NOTE: this is part of the graphdb.RTx interface.
func (t *rpcRTx) Close() error {
	return nil
}

// MustImplementRTx is a helper method that ensures that the kvdbRTx type
// implements the RTx interface.
//
// NOTE: this is part of the graphdb.RTx interface.
func (t *rpcRTx) MustImplementRTx() {}

// A compile-time assertion to ensure that kvdbRTx implements the RTx interface.
var _ session.RTx = (*rpcRTx)(nil)

// bootstrapper is a NetworkPeerBootstrapper implementation that is backed by a
// rpc Client graph source.
type bootstrapper struct {
	name string
	*Client
}

// SampleNodeAddrs queries the remote node for a uniform sample set of address
// from its network peer bootstrapper source. The num addrs field passed in
// denotes how many valid peer addresses to return. The passed set of node
// nodes allows the caller to ignore a set of nodes perhaps because they
// already have connections established.
//
// NOTE: This is part of the discovery.NetworkPeerBootstrapper interface.
func (r *bootstrapper) SampleNodeAddrs(ctx context.Context, numAddrs uint32,
	ignore map[autopilot.NodeID]struct{}) ([]*lnwire.NetAddress, error) {

	var toIgnore [][]byte
	for id := range ignore {
		toIgnore = append(toIgnore, id[:])
	}

	resp, err := r.graphConn.BootstrapAddrs(
		ctx, &BootstrapAddrsReq{
			NumAddrs:    numAddrs,
			IgnoreNodes: toIgnore,
		},
	)
	if err != nil {
		return nil, err
	}

	var netAddrs []*lnwire.NetAddress
	for nodeIDStr, addrs := range resp.Addresses {
		nodeIDBytes, err := hex.DecodeString(nodeIDStr)
		if err != nil {
			return nil, err
		}

		nodeID, err := btcec.ParsePubKey(nodeIDBytes)
		if err != nil {
			return nil, err
		}

		for _, addr := range addrs.Addresses {
			netAddr, err := lncfg.ParseAddressString(
				addr.Addr, strconv.Itoa(lncfg.DefaultPeerPort),
				r.Client.resolveTCPAddr,
			)
			if err != nil {
				return nil, err
			}

			netAddrs = append(netAddrs, &lnwire.NetAddress{
				IdentityKey: nodeID,
				Address:     netAddr,
			})
		}
	}

	return netAddrs, nil
}

// Name returns the name of the network peer bootstrapper implementation used
// by the remote node.
//
// NOTE: This is part of the discovery.NetworkPeerBootstrapper interface.
func (r *bootstrapper) Name(_ context.Context) string {
	return r.name
}

func unmarshalFeatures(
	features map[uint32]*lnrpc.Feature) *lnwire.FeatureVector {

	featureBits := make([]lnwire.FeatureBit, 0, len(features))
	featureNames := make(map[lnwire.FeatureBit]string)
	for featureBit, feature := range features {
		featureBits = append(
			featureBits, lnwire.FeatureBit(featureBit),
		)

		featureNames[lnwire.FeatureBit(featureBit)] = feature.Name
	}

	return lnwire.NewFeatureVector(
		lnwire.NewRawFeatureVector(featureBits...), featureNames,
	)
}

func unmarshalChannelInfo(info *lnrpc.ChannelEdge) (*models.ChannelEdgeInfo,
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

	edge := &models.ChannelEdgeInfo{
		ChannelID:       info.ChannelId,
		ChannelPoint:    *chanPoint,
		NodeKey1Bytes:   node1Bytes,
		NodeKey2Bytes:   node2Bytes,
		Capacity:        btcutil.Amount(info.Capacity),
		ExtraOpaqueData: extra,
	}

	// To ensure that Describe graph doesn't filter it out as a private
	// channel, we set the auth proof to a non-nil value if the channel
	// has been announced.
	if info.Announced {
		edge.AuthProof = &models.ChannelAuthProof{}
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

func unmarshalPolicy(channelID uint64, rpcPolicy *lnrpc.RoutingPolicy,
	node1 bool, toNodeStr string) (*models.ChannelEdgePolicy, error) {

	var chanFlags lnwire.ChanUpdateChanFlags
	if !node1 {
		chanFlags |= lnwire.ChanUpdateDirection
	}
	if rpcPolicy.Disabled {
		chanFlags |= lnwire.ChanUpdateDisabled
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

	return &models.ChannelEdgePolicy{
		ChannelID:     channelID,
		TimeLockDelta: uint16(rpcPolicy.TimeLockDelta),
		MinHTLC: lnwire.MilliSatoshi(
			rpcPolicy.MinHtlc,
		),
		MaxHTLC: lnwire.MilliSatoshi(
			rpcPolicy.MaxHtlcMsat,
		),
		FeeBaseMSat: lnwire.MilliSatoshi(
			rpcPolicy.FeeBaseMsat,
		),
		FeeProportionalMillionths: lnwire.MilliSatoshi(
			rpcPolicy.FeeRateMilliMsat,
		),
		LastUpdate:      time.Unix(int64(rpcPolicy.LastUpdate), 0),
		ChannelFlags:    chanFlags,
		ToNode:          toNode,
		ExtraOpaqueData: extra,
	}, nil
}

func (r *Client) unmarshalAddrs(addrs []*lnrpc.NodeAddress) ([]net.Addr,
	error) {

	netAddrs := make([]net.Addr, 0, len(addrs))
	for _, addr := range addrs {
		netAddr, err := lncfg.ParseAddressString(
			addr.Addr, strconv.Itoa(lncfg.DefaultPeerPort),
			r.resolveTCPAddr,
		)
		if err != nil {
			return nil, err
		}
		netAddrs = append(netAddrs, netAddr)
	}

	return netAddrs, nil
}
