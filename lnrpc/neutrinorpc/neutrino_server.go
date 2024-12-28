//go:build neutrinorpc
// +build neutrinorpc

package neutrinorpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/blockchain"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize it as the name of our
	// RPC service.
	subServerName = "NeutrinoKitRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/neutrinorpc.NeutrinoKit/Status": {{
			Entity: "info",
			Action: "read",
		}},
		"/neutrinorpc.NeutrinoKit/AddPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/neutrinorpc.NeutrinoKit/DisconnectPeer": {{
			Entity: "peers",
			Action: "write",
		}},
		"/neutrinorpc.NeutrinoKit/IsBanned": {{
			Entity: "info",
			Action: "read",
		}},
		"/neutrinorpc.NeutrinoKit/GetBlock": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/neutrinorpc.NeutrinoKit/GetBlockHeader": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/neutrinorpc.NeutrinoKit/GetCFilter": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/neutrinorpc.NeutrinoKit/GetBlockHash": {{
			Entity: "onchain",
			Action: "read",
		}},
	}

	// ErrNeutrinoNotActive is an error returned when there is no running
	// neutrino light client instance.
	ErrNeutrinoNotActive = errors.New("no active neutrino instance")
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	NeutrinoKitServer
}

// Server is a sub-server of the main RPC server: the neutrino RPC. This sub
// RPC server allows external callers to access the status of the neutrino
// currently active within lnd, as well as configuring it at runtime.
type Server struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	// Must be after the atomically used variables to not break struct
	// alignment.
	UnimplementedNeutrinoKitServer

	cfg *Config
}

// A compile time check to ensure that NeutrinoKit fully implements the
// NeutrinoServer gRPC service.
var _ NeutrinoKitServer = (*Server)(nil)

// New returns a new instance of the neutrinorpc Neutrino sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// We don't create any new macaroons for this subserver, instead reuse
	// existing onchain/offchain permissions.
	server := &Server{
		cfg: cfg,
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have
// requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterNeutrinoKitServer(grpcServer, r)

	log.Debugf("Neutrino RPC server successfully registered with root " +
		"gRPC server")

	return nil
}

// RegisterWithRestServer will be called by the root REST mux to direct a sub
// RPC server to register itself with the main REST mux server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRestServer(ctx context.Context,
	mux *runtime.ServeMux, dest string, opts []grpc.DialOption) error {

	// We make sure that we register it with the main REST server to ensure
	// all our methods are routed properly.
	err := RegisterNeutrinoKitHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register Neutrino REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("Neutrino REST server successfully registered with " +
		"root REST server")
	return nil
}

// CreateSubServer populates the subserver's dependencies using the passed
// SubServerConfigDispatcher. This method should fully initialize the
// sub-server instance, making it ready for action. It returns the macaroon
// permissions that the sub-server wishes to pass on to the root server for all
// methods routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) CreateSubServer(configRegistry lnrpc.SubServerConfigDispatcher) (
	lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	subServer, macPermissions, err := createNewSubServer(configRegistry)
	if err != nil {
		return nil, nil, err
	}

	r.NeutrinoKitServer = subServer
	return subServer, macPermissions, nil
}

// Status returns the current status, best block height and connected peers
// of the neutrino node.
//
// NOTE: Part of the NeutrinoServer interface.
func (s *Server) Status(ctx context.Context,
	in *StatusRequest) (*StatusResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	bestBlock, err := s.cfg.NeutrinoCS.BestBlock()
	if err != nil {
		return nil, fmt.Errorf("could not get best block: %w", err)
	}

	peers := s.cfg.NeutrinoCS.Peers()
	var Peers = make([]string, len(peers))
	for i, p := range peers {
		Peers[i] = p.Addr()
	}

	return &StatusResponse{
		Active:      s.cfg.NeutrinoCS != nil,
		BlockHeight: bestBlock.Height,
		BlockHash:   bestBlock.Hash.String(),
		Synced:      s.cfg.NeutrinoCS.IsCurrent(),
		Peers:       Peers,
	}, nil
}

// AddPeer adds a new peer that has already been connected to the server.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) AddPeer(ctx context.Context,
	in *AddPeerRequest) (*AddPeerResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	peer := s.cfg.NeutrinoCS.PeerByAddr(in.PeerAddrs)
	if peer == nil {
		return nil,
			fmt.Errorf("could not found peer: %s", in.PeerAddrs)
	}
	s.cfg.NeutrinoCS.AddPeer(peer)

	return &AddPeerResponse{}, nil
}

// DisconnectPeer disconnects a peer by target address. Both outbound and
// inbound nodes will be searched for the target node. An error message will
// be returned if the peer was not found.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) DisconnectPeer(ctx context.Context,
	in *DisconnectPeerRequest) (*DisconnectPeerResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	peer := s.cfg.NeutrinoCS.PeerByAddr(in.PeerAddrs)
	if peer == nil {
		return nil,
			fmt.Errorf("could not found peer: %s", in.PeerAddrs)
	}

	err := s.cfg.NeutrinoCS.DisconnectNodeByAddr(peer.Addr())
	if err != nil {
		return nil, err
	}

	return &DisconnectPeerResponse{}, nil
}

// IsBanned returns true if the peer is banned, otherwise false.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) IsBanned(ctx context.Context,
	in *IsBannedRequest) (*IsBannedResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	return &IsBannedResponse{
		Banned: s.cfg.NeutrinoCS.IsBanned(in.PeerAddrs),
	}, nil
}

// GetBlockHeader returns a block header with a particular block hash. If the
// block header is found in the cache, it will be returned immediately.
// Otherwise a block will  be requested from the network, one peer at a time,
// until one answers.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) GetBlockHeader(ctx context.Context,
	in *GetBlockHeaderRequest) (*GetBlockHeaderResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	var hash chainhash.Hash
	if err := chainhash.Decode(&hash, in.Hash); err != nil {
		return nil, err
	}

	resp, err := s.getBlock(hash)
	if err != nil {
		return nil, err
	}

	return &GetBlockHeaderResponse{
		Hash:              resp.Hash,
		Confirmations:     resp.Confirmations,
		StrippedSize:      resp.StrippedSize,
		Size:              resp.Size,
		Weight:            resp.Weight,
		Height:            resp.Height,
		Version:           resp.Version,
		VersionHex:        resp.VersionHex,
		Merkleroot:        resp.Merkleroot,
		Time:              resp.Time,
		Nonce:             resp.Nonce,
		Bits:              resp.Bits,
		Ntx:               resp.Ntx,
		PreviousBlockHash: resp.PreviousBlockHash,
		RawHex:            resp.RawHex,
	}, nil
}

// GetBlock returns a block with a particular block hash. If the block is
// found in the cache, it will be returned immediately. Otherwise a block will
// be requested from the network, one peer at a time, until one answers.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) GetBlock(ctx context.Context,
	in *GetBlockRequest) (*GetBlockResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	var hash chainhash.Hash
	if err := chainhash.Decode(&hash, in.Hash); err != nil {
		return nil, err
	}

	return s.getBlock(hash)
}

// GetCFilter returns a compact filter of a particular block.
// If found, only regular filters will be returned.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) GetCFilter(ctx context.Context,
	in *GetCFilterRequest) (*GetCFilterResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	var hash chainhash.Hash
	if err := chainhash.Decode(&hash, in.Hash); err != nil {
		return nil, err
	}

	// GetCFilter returns a compact filter from the database. If it is
	// missing, it requests the compact filter from the network.
	filter, err := s.cfg.NeutrinoCS.GetCFilter(hash, wire.GCSFilterRegular)
	if err != nil {
		return nil, err
	}

	filterlBytes, err := filter.Bytes()
	if err != nil {
		return nil, err
	}

	return &GetCFilterResponse{Filter: filterlBytes}, nil
}

func (s *Server) getBlock(hash chainhash.Hash) (*GetBlockResponse, error) {
	block, err := s.cfg.NeutrinoCS.GetBlock(hash)
	if err != nil {
		return nil, err
	}

	header, _, err := s.cfg.NeutrinoCS.BlockHeaders.FetchHeader(&hash)
	if err != nil {
		return nil, err
	}

	blockData, err := block.Bytes()
	if err != nil {
		return nil, err
	}

	strippedData, err := block.BytesNoWitness()
	if err != nil {
		return nil, err
	}

	bestBlock, err := s.cfg.NeutrinoCS.BestBlock()
	if err != nil {
		return nil, err
	}

	// Convert txids to a string array.
	transactions := block.Transactions()
	tx := make([]string, len(transactions))
	for i := range transactions {
		tx[i] = transactions[i].Hash().String()
	}

	return &GetBlockResponse{
		Hash:          block.Hash().String(),
		Confirmations: int64(1 + bestBlock.Height - block.Height()),
		StrippedSize:  int64(len(strippedData)),
		Size:          int64(len(blockData)),
		Weight:        blockchain.GetBlockWeight(block),
		Height:        block.Height(),
		Version:       header.Version,
		VersionHex:    fmt.Sprintf("%0x", header.Version),
		Merkleroot:    header.MerkleRoot.String(),
		Tx:            tx,
		Time:          header.Timestamp.Unix(),
		Nonce:         header.Nonce,
		// Format bits as a hex.
		Bits:              fmt.Sprintf("%0x", header.Bits),
		Ntx:               int32(len(block.Transactions())),
		PreviousBlockHash: header.PrevBlock.String(),
		RawHex:            blockData,
	}, nil
}

// GetBlockHash returns the header hash of a block at a given height.
//
// NOTE: Part of the NeutrinoKitServer interface.
func (s *Server) GetBlockHash(ctx context.Context,
	in *GetBlockHashRequest) (*GetBlockHashResponse, error) {

	if s.cfg.NeutrinoCS == nil {
		return nil, ErrNeutrinoNotActive
	}

	hash, err := s.cfg.NeutrinoCS.GetBlockHash(int64(in.Height))
	if err != nil {
		return nil, err
	}

	return &GetBlockHashResponse{Hash: hash.String()}, nil
}
