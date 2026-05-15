//go:build chainrpc
// +build chainrpc

package chainrpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the RPC sub-server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize this as the name of the
	// config file that we need.
	subServerName = "ChainRPC"

	// pkScriptRPCSendQueueSize is the maximum number of pkScript events that
	// may wait for the stream writer before the RPC applies slow-client
	// backpressure.
	pkScriptRPCSendQueueSize = 100

	// pkScriptRPCSendTimeout is the maximum time a pkScript RPC send may wait
	// for a client to accept response events.
	pkScriptRPCSendTimeout = 30 * time.Second
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "onchain",
			Action: "read",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/chainrpc.ChainKit/GetBlock": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainKit/GetBlockHeader": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainKit/GetBestBlock": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainKit/GetBlockHash": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainNotifier/RegisterConfirmationsNtfn": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainNotifier/RegisterSpendNtfn": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainNotifier/RegisterBlockEpochNtfn": {{
			Entity: "onchain",
			Action: "read",
		}},
		"/chainrpc.ChainNotifier/RegisterPkScriptNtfn": {{
			Entity: "onchain",
			Action: "read",
		}},
	}

	// DefaultChainNotifierMacFilename is the default name of the chain
	// notifier macaroon that we expect to find via a file handle within the
	// main configuration file in this package.
	DefaultChainNotifierMacFilename = "chainnotifier.macaroon"

	// ErrChainNotifierServerShuttingDown is an error returned when we are
	// waiting for a notification to arrive but the chain notifier server
	// has been shut down.
	ErrChainNotifierServerShuttingDown = errors.New("chain notifier RPC " +
		"subserver shutting down")

	// ErrChainNotifierServerNotActive indicates that the chain notifier hasn't
	// finished the startup process.
	ErrChainNotifierServerNotActive = status.Error(codes.Unavailable,
		"chain notifier RPC is still in the process of starting")
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	ChainKitServer
	ChainNotifierServer
}

// Server is a sub-server of the main RPC server. It serves the chainkit RPC
// and chain notifier RPC. This RPC sub-server allows external callers to access
// the full chainkit and chain notifier capabilities of lnd. This allows callers
// to create custom protocols, external to lnd, even backed by multiple distinct
// lnd across independent failure domains.
type Server struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	UnimplementedChainNotifierServer
	UnimplementedChainKitServer

	started sync.Once
	stopped sync.Once

	cfg Config

	quit chan struct{}
}

// New returns a new instance of the chainrpc ChainNotifier sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the chain notifier macaroon wasn't generated, then
	// we'll assume that it's found at the default network directory.
	if cfg.ChainNotifierMacPath == "" {
		cfg.ChainNotifierMacPath = filepath.Join(
			cfg.NetworkDir, DefaultChainNotifierMacFilename,
		)
	}

	// Now that we know the full path of the chain notifier macaroon, we can
	// check to see if we need to create it or not. If stateless_init is set
	// then we don't write the macaroons.
	macFilePath := cfg.ChainNotifierMacPath
	if cfg.MacService != nil && !cfg.MacService.StatelessInit &&
		!lnrpc.FileExists(macFilePath) {

		log.Infof("Baking macaroons for ChainNotifier RPC Server at: %v",
			macFilePath)

		// At this point, we know that the chain notifier macaroon
		// doesn't yet, exist, so we need to create it with the help of
		// the main macaroon service.
		chainNotifierMac, err := cfg.MacService.NewMacaroon(
			context.Background(), macaroons.DefaultRootKeyID,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		chainNotifierMacBytes, err := chainNotifierMac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = os.WriteFile(macFilePath, chainNotifierMacBytes, 0644)
		if err != nil {
			_ = os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	return &Server{
		cfg:  *cfg,
		quit: make(chan struct{}),
	}, macPermissions, nil
}

// Compile-time checks to ensure that Server fully implements the
// ChainNotifierServer gRPC service, ChainKitServer gRPC service, and
// lnrpc.SubServer interface.
var _ ChainNotifierServer = (*Server)(nil)
var _ ChainKitServer = (*Server)(nil)
var _ lnrpc.SubServer = (*Server)(nil)

// Start launches any helper goroutines required for the server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	s.started.Do(func() {})
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	s.stopped.Do(func() {
		close(s.quit)
	})
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a RPC
// sub-server to register itself with the main gRPC root server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterChainNotifierServer(grpcServer, r)
	log.Debug("ChainNotifier RPC server successfully registered with " +
		"root gRPC server")

	RegisterChainKitServer(grpcServer, r)
	log.Debug("ChainKit RPC server successfully registered with root " +
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
	err := RegisterChainNotifierHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register ChainNotifier REST server "+
			"with root REST server: %v", err)
		return err
	}

	log.Debugf("ChainNotifier REST server successfully registered with " +
		"root REST server")

	// Register chainkit with the main REST server to ensure all our methods
	// are routed properly.
	err = RegisterChainKitHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		log.Errorf("Could not register ChainKit REST server with root "+
			"REST server: %v", err)
		return err
	}
	log.Debugf("ChainKit REST server successfully registered with root " +
		"REST server")

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

	r.ChainNotifierServer = subServer
	r.ChainKitServer = subServer
	return subServer, macPermissions, nil
}

// GetBlock returns a block given the corresponding block hash.
func (s *Server) GetBlock(_ context.Context,
	in *GetBlockRequest) (*GetBlockResponse, error) {

	// We'll start by reconstructing the RPC request into what the
	// underlying chain functionality expects.
	var blockHash chainhash.Hash
	copy(blockHash[:], in.BlockHash)

	block, err := s.cfg.Chain.GetBlock(&blockHash)
	if err != nil {
		return nil, err
	}

	// Serialize block for RPC response.
	var blockBuf bytes.Buffer
	err = block.Serialize(&blockBuf)
	if err != nil {
		return nil, err
	}
	rawBlock := blockBuf.Bytes()

	return &GetBlockResponse{RawBlock: rawBlock}, nil
}

// GetBlockHeader returns a block header given the corresponding block hash.
func (s *Server) GetBlockHeader(_ context.Context,
	in *GetBlockHeaderRequest) (*GetBlockHeaderResponse, error) {

	// We'll start by reconstructing the RPC request into what the
	// underlying chain functionality expects.
	var blockHash chainhash.Hash
	copy(blockHash[:], in.BlockHash)

	blockHeader, err := s.cfg.Chain.GetBlockHeader(&blockHash)
	if err != nil {
		return nil, err
	}

	// Serialize block header for RPC response.
	var headerBuf bytes.Buffer
	err = blockHeader.Serialize(&headerBuf)
	if err != nil {
		return nil, err
	}
	rawHeader := headerBuf.Bytes()

	return &GetBlockHeaderResponse{RawBlockHeader: rawHeader}, nil
}

// GetBestBlock returns the latest block hash and current height of the valid
// most-work chain.
func (s *Server) GetBestBlock(_ context.Context,
	_ *GetBestBlockRequest) (*GetBestBlockResponse, error) {

	blockHash, blockHeight, err := s.cfg.Chain.GetBestBlock()
	if err != nil {
		return nil, err
	}

	return &GetBestBlockResponse{
		BlockHash:   blockHash[:],
		BlockHeight: blockHeight,
	}, nil
}

// GetBlockHash returns the hash of the block in the best blockchain
// at the given height.
func (s *Server) GetBlockHash(_ context.Context,
	req *GetBlockHashRequest) (*GetBlockHashResponse, error) {

	blockHash, err := s.cfg.Chain.GetBlockHash(req.BlockHeight)
	if err != nil {
		return nil, err
	}

	return &GetBlockHashResponse{
		BlockHash: blockHash[:],
	}, nil
}

// RegisterConfirmationsNtfn is a synchronous response-streaming RPC that
// registers an intent for a client to be notified once a confirmation request
// has reached its required number of confirmations on-chain.
//
// A client can specify whether the confirmation request should be for a
// particular transaction by its hash or for an output script by specifying a
// zero hash.
//
// NOTE: This is part of the chainrpc.ChainNotifierServer interface.
func (s *Server) RegisterConfirmationsNtfn(in *ConfRequest,
	confStream ChainNotifier_RegisterConfirmationsNtfnServer) error {

	if !s.cfg.ChainNotifier.Started() {
		return ErrChainNotifierServerNotActive
	}

	// We'll start by reconstructing the RPC request into what the
	// underlying ChainNotifier expects.
	var txid chainhash.Hash
	copy(txid[:], in.Txid)

	var opts []chainntnfs.NotifierOption
	if in.IncludeBlock {
		opts = append(opts, chainntnfs.WithIncludeBlock())
	}

	// We'll then register for the spend notification of the request.
	confEvent, err := s.cfg.ChainNotifier.RegisterConfirmationsNtfn(
		&txid, in.Script, in.NumConfs, in.HeightHint, opts...,
	)
	if err != nil {
		return err
	}
	defer confEvent.Cancel()

	// With the request registered, we'll wait for its spend notification to
	// be dispatched.
	for {
		select {
		// The transaction satisfying the request has confirmed on-chain
		// and reached its required number of confirmations. We'll
		// dispatch an event to the caller indicating so.
		case details, ok := <-confEvent.Confirmed:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			var rawTxBuf bytes.Buffer
			err := details.Tx.Serialize(&rawTxBuf)
			if err != nil {
				return err
			}

			// If the block was included (should only be there if
			// IncludeBlock is true), then we'll encode the bytes
			// to send with the response.
			var blockBytes []byte
			if details.Block != nil {
				var blockBuf bytes.Buffer
				err := details.Block.Serialize(&blockBuf)
				if err != nil {
					return err
				}

				blockBytes = blockBuf.Bytes()
			}

			rpcConfDetails := &ConfDetails{
				RawTx:       rawTxBuf.Bytes(),
				BlockHash:   details.BlockHash[:],
				BlockHeight: details.BlockHeight,
				TxIndex:     details.TxIndex,
				RawBlock:    blockBytes,
			}

			conf := &ConfEvent{
				Event: &ConfEvent_Conf{
					Conf: rpcConfDetails,
				},
			}
			if err := confStream.Send(conf); err != nil {
				return err
			}

		// The transaction satisfying the request has been reorged out
		// of the chain, so we'll send an event describing it.
		case _, ok := <-confEvent.NegativeConf:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			reorg := &ConfEvent{
				Event: &ConfEvent_Reorg{Reorg: &Reorg{}},
			}
			if err := confStream.Send(reorg); err != nil {
				return err
			}

		// The transaction satisfying the request has confirmed and is
		// no longer under the risk of being reorged out of the chain,
		// so we can safely exit.
		case _, ok := <-confEvent.Done:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			return nil

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-confStream.Context().Done():
			if errors.Is(confStream.Context().Err(), context.Canceled) {
				return nil
			}
			return confStream.Context().Err()

		// The server has been requested to shut down.
		case <-s.quit:
			return ErrChainNotifierServerShuttingDown
		}
	}
}

// RegisterSpendNtfn is a synchronous response-streaming RPC that registers an
// intent for a client to be notification once a spend request has been spent by
// a transaction that has confirmed on-chain.
//
// A client can specify whether the spend request should be for a particular
// outpoint  or for an output script by specifying a zero outpoint.
//
// NOTE: This is part of the chainrpc.ChainNotifierServer interface.
func (s *Server) RegisterSpendNtfn(in *SpendRequest,
	spendStream ChainNotifier_RegisterSpendNtfnServer) error {

	if !s.cfg.ChainNotifier.Started() {
		return ErrChainNotifierServerNotActive
	}

	// We'll start by reconstructing the RPC request into what the
	// underlying ChainNotifier expects.
	var op *wire.OutPoint
	if in.Outpoint != nil {
		var txid chainhash.Hash
		copy(txid[:], in.Outpoint.Hash)
		op = &wire.OutPoint{Hash: txid, Index: in.Outpoint.Index}
	}

	// We'll then register for the spend notification of the request.
	spendEvent, err := s.cfg.ChainNotifier.RegisterSpendNtfn(
		op, in.Script, in.HeightHint,
	)
	if err != nil {
		return err
	}
	defer spendEvent.Cancel()

	// With the request registered, we'll wait for its spend notification to
	// be dispatched.
	for {
		select {
		// A transaction that spends the given has confirmed on-chain.
		// We'll return an event to the caller indicating so that
		// includes the details of the spending transaction.
		case details, ok := <-spendEvent.Spend:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			var rawSpendingTxBuf bytes.Buffer
			err := details.SpendingTx.Serialize(&rawSpendingTxBuf)
			if err != nil {
				return err
			}

			rpcSpendDetails := &SpendDetails{
				SpendingOutpoint: &Outpoint{
					Hash:  details.SpentOutPoint.Hash[:],
					Index: details.SpentOutPoint.Index,
				},
				RawSpendingTx:      rawSpendingTxBuf.Bytes(),
				SpendingTxHash:     details.SpenderTxHash[:],
				SpendingInputIndex: details.SpenderInputIndex,
				SpendingHeight:     uint32(details.SpendingHeight),
			}

			spend := &SpendEvent{
				Event: &SpendEvent_Spend{
					Spend: rpcSpendDetails,
				},
			}
			if err := spendStream.Send(spend); err != nil {
				return err
			}

		// The spending transaction of the request has been reorged of
		// the chain. We'll return an event to the caller indicating so.
		case _, ok := <-spendEvent.Reorg:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			reorg := &SpendEvent{
				Event: &SpendEvent_Reorg{Reorg: &Reorg{}},
			}
			if err := spendStream.Send(reorg); err != nil {
				return err
			}

		// The spending transaction of the requests has confirmed
		// on-chain and is no longer under the risk of being reorged out
		// of the chain, so we can safely exit.
		case _, ok := <-spendEvent.Done:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			return nil

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-spendStream.Context().Done():
			if errors.Is(spendStream.Context().Err(), context.Canceled) {
				return nil
			}
			return spendStream.Context().Err()

		// The server has been requested to shut down.
		case <-s.quit:
			return ErrChainNotifierServerShuttingDown
		}
	}
}

// rpcPkScriptEventTypesToNotifier converts RPC event type filters into
// chainntnfs pkScript event flags.
func rpcPkScriptEventTypesToNotifier(eventTypes []PkScriptEventType) (
	chainntnfs.PkScriptEventType, error) {

	var ntfnType chainntnfs.PkScriptEventType
	for _, eventType := range eventTypes {
		switch eventType {
		case PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRM:
			ntfnType |= chainntnfs.PkScriptEventConfirm

		case PkScriptEventType_PK_SCRIPT_EVENT_TYPE_SPEND:
			ntfnType |= chainntnfs.PkScriptEventSpend

		case PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRMATION_UPDATE:
			return 0, status.Error(
				codes.InvalidArgument,
				"confirmation updates are requested with "+
					"include_confirmation_updates",
			)

		default:
			return 0, status.Errorf(
				codes.InvalidArgument,
				"unknown pkScript event type: %v", eventType,
			)
		}
	}

	return ntfnType, nil
}

// rpcAddPkScriptOptions converts an RPC add request into notifier options.
func rpcAddPkScriptOptions(add *AddPkScriptRequest) (
	[]chainntnfs.NotifierOption, error) {

	if add == nil {
		return nil, status.Error(
			codes.InvalidArgument, "add request must be set",
		)
	}

	var opts []chainntnfs.NotifierOption

	if len(add.Events) > 0 {
		events, err := rpcPkScriptEventTypesToNotifier(add.Events)
		if err != nil {
			return nil, err
		}
		opts = append(opts, chainntnfs.WithEvents(events))
	}
	if add.NumConfs != 0 {
		opts = append(opts, chainntnfs.WithNumConfs(add.NumConfs))
	}
	if add.IncludeBlock {
		opts = append(opts, chainntnfs.WithIncludeBlock())
	}
	if add.IncludeTx {
		opts = append(opts, chainntnfs.WithIncludeTx())
	}
	if add.IncludeConfirmationUpdates {
		opts = append(opts, chainntnfs.WithIncludeConfirmationUpdates())
	}
	scanReq := add.HistoricalScan
	historicalScan, ok := scanReq.(*AddPkScriptRequest_HistoricalScanFrom)
	if ok {
		opts = append(
			opts, chainntnfs.WithHistoricalScanFrom(
				historicalScan.HistoricalScanFrom,
			),
		)
	}

	return opts, nil
}

// rpcPkScriptNotification converts a chain notifier pkScript notification into
// its RPC representation.
func rpcPkScriptNotification(ntfn *chainntnfs.PkScriptNotification) (
	*PkScriptNotification, error) {

	if ntfn == nil {
		return nil, nil
	}

	rpcNtfn := &PkScriptNotification{
		Height:                ntfn.Height,
		TxIndex:               ntfn.TxIndex,
		InputIndex:            ntfn.InputIndex,
		NumConfirmations:      ntfn.NumConfirmations,
		RequiredConfirmations: ntfn.RequiredConfs,
		Disconnected:          ntfn.Disconnected,
	}

	switch ntfn.Type {
	case chainntnfs.PkScriptNotificationConfirm:
		rpcNtfn.EventType = PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRM

	case chainntnfs.PkScriptNotificationSpend:
		rpcNtfn.EventType = PkScriptEventType_PK_SCRIPT_EVENT_TYPE_SPEND

	case chainntnfs.PkScriptNotificationConfirmUpdate:
		rpcNtfn.EventType =
			PkScriptEventType_PK_SCRIPT_EVENT_TYPE_CONFIRMATION_UPDATE

	default:
		return nil, status.Errorf(
			codes.Internal, "unknown pkScript notification type: %v",
			ntfn.Type,
		)
	}

	if ntfn.BlockHash != nil {
		rpcNtfn.BlockHash = ntfn.BlockHash[:]
	}
	if ntfn.TxHash != nil {
		rpcNtfn.TxHash = ntfn.TxHash[:]
	}
	if ntfn.UTXO != nil {
		rpcNtfn.Utxo = &PkScriptUtxo{
			Outpoint: &Outpoint{
				Hash:  ntfn.UTXO.OutPoint.Hash[:],
				Index: ntfn.UTXO.OutPoint.Index,
			},
			Value:       int64(ntfn.UTXO.Value),
			PkScript:    ntfn.UTXO.PkScript,
			BlockHeight: ntfn.UTXO.BlockHeight,
			TxIndex:     ntfn.UTXO.TxIndex,
		}
		if ntfn.UTXO.BlockHash != nil {
			rpcNtfn.Utxo.BlockHash = ntfn.UTXO.BlockHash[:]
		}
	}

	if ntfn.Tx != nil {
		var txBuf bytes.Buffer
		err := ntfn.Tx.Serialize(&txBuf)
		if err != nil {
			return nil, err
		}
		rpcNtfn.RawTx = txBuf.Bytes()
	}

	if ntfn.Block != nil {
		var blockBuf bytes.Buffer
		err := ntfn.Block.Serialize(&blockBuf)
		if err != nil {
			return nil, err
		}
		rpcNtfn.RawBlock = blockBuf.Bytes()
	}

	return rpcNtfn, nil
}

// rpcPkScriptHistoricalScan converts historical scan lifecycle data into its
// RPC representation.
func rpcPkScriptHistoricalScan(
	scan *chainntnfs.PkScriptHistoricalScan) *PkScriptHistoricalScan {

	if scan == nil {
		return nil
	}

	return &PkScriptHistoricalScan{
		ScanId:          scan.ScanID,
		StartHeight:     scan.StartHeight,
		EndHeight:       scan.EndHeight,
		CompletedHeight: scan.CompletedHeight,
		Error:           scan.Error,
	}
}

// rpcPkScriptAck converts mutation result metadata into an RPC acknowledgement.
func rpcPkScriptAck(action PkScriptMutationAction,
	result *chainntnfs.PkScriptAddResult) *PkScriptMutationAck {

	ack := &PkScriptMutationAck{
		Action: action,
	}
	if result == nil {
		return ack
	}

	ack.NumAdded = result.NumAdded
	ack.HistoricalScanQueued = result.HistoricalScanQueued
	ack.HistoricalScanId = result.HistoricalScanID
	ack.HistoricalScanStartHeight = result.HistoricalScanStartHeight
	ack.HistoricalScanEndHeight = result.HistoricalScanEndHeight

	return ack
}

type pkScriptRPCSendRequest struct {
	event  *PkScriptEvent
	result chan error
}

type pkScriptRPCEventSender struct {
	stream   ChainNotifier_RegisterPkScriptNtfnServer
	quit     <-chan struct{}
	done     chan struct{}
	stopOnce sync.Once

	queue chan *pkScriptRPCSendRequest
	err   chan error

	timeout time.Duration
}

// newPkScriptRPCEventSender creates a serialized sender for pkScript RPC stream
// events.
func newPkScriptRPCEventSender(
	stream ChainNotifier_RegisterPkScriptNtfnServer,
	quit <-chan struct{}, timeout time.Duration) *pkScriptRPCEventSender {

	sender := &pkScriptRPCEventSender{
		stream:  stream,
		quit:    quit,
		done:    make(chan struct{}),
		queue:   make(chan *pkScriptRPCSendRequest, pkScriptRPCSendQueueSize),
		err:     make(chan error, 1),
		timeout: timeout,
	}

	go sender.run()

	return sender
}

// stop terminates the RPC event sender.
func (s *pkScriptRPCEventSender) stop() {
	s.stopOnce.Do(func() {
		close(s.done)
	})
}

// reportErr stores the first RPC send error observed by the sender.
func (s *pkScriptRPCEventSender) reportErr(err error) {
	if err == nil {
		return
	}

	select {
	case s.err <- err:
	default:
	}
}

// run serializes queued stream.Send calls.
func (s *pkScriptRPCEventSender) run() {
	for {
		select {
		case req := <-s.queue:
			select {
			case <-s.done:
				return
			default:
			}

			err := s.stream.Send(req.event)
			s.reportErr(err)

			select {
			case req.result <- err:
			case <-s.done:
				return
			}

			if err != nil {
				return
			}

		case <-s.done:
			return
		}
	}
}

// send queues an event and waits for it to be sent or timed out.
func (s *pkScriptRPCEventSender) send(event *PkScriptEvent) error {
	req := &pkScriptRPCSendRequest{
		event:  event,
		result: make(chan error, 1),
	}

	timer := time.NewTimer(s.timeout)
	defer timer.Stop()

	select {
	case s.queue <- req:

	case err := <-s.err:
		return err

	case <-timer.C:
		return status.Error(
			codes.ResourceExhausted,
			"pkScript notification client is not reading responses",
		)

	case <-s.stream.Context().Done():
		return s.stream.Context().Err()

	case <-s.quit:
		return ErrChainNotifierServerShuttingDown
	}

	select {
	case err := <-req.result:
		return err

	case err := <-s.err:
		return err

	case <-timer.C:
		return status.Error(
			codes.ResourceExhausted,
			"pkScript notification client is not reading responses",
		)

	case <-s.stream.Context().Done():
		return s.stream.Context().Err()

	case <-s.quit:
		return ErrChainNotifierServerShuttingDown
	}
}

// pkScriptNotificationStreamClosedErr maps the notifier-side terminal reason for
// a closed pkScript notification stream into the RPC error returned to the
// client.
func pkScriptNotificationStreamClosedErr(
	reg *chainntnfs.PkScriptNotificationRegistration) error {

	if reg != nil && reg.Err != nil {
		err := reg.Err()
		if errors.Is(err, chainntnfs.ErrPkScriptNotificationQueueFull) {
			return status.Error(
				codes.ResourceExhausted,
				"pkScript notification client is not reading responses",
			)
		}
		if err != nil && !errors.Is(err, chainntnfs.ErrTxNotifierExiting) {
			return err
		}
	}

	return chainntnfs.ErrChainNotifierShuttingDown
}

// RegisterPkScriptNtfn is a bidirectional streaming RPC that registers a
// pkScript notification stream and allows the caller to add/remove pkScripts on
// the fly over the same gRPC stream.
//
// NOTE: This is part of the chainrpc.ChainNotifierServer interface.
func (s *Server) RegisterPkScriptNtfn(
	stream ChainNotifier_RegisterPkScriptNtfnServer) error {

	if !s.cfg.ChainNotifier.Started() {
		return ErrChainNotifierServerNotActive
	}

	firstReq, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(
				codes.InvalidArgument,
				"first pkScript request must be a register action",
			)
		}

		return err
	}

	registerReq := firstReq.GetRegister()
	if registerReq == nil {
		return status.Error(
			codes.InvalidArgument,
			"first pkScript request must be a register action",
		)
	}

	reg, err := s.cfg.PkScriptNotifier.RegisterPkScriptNotifier()
	if err != nil {
		return err
	}
	defer reg.Cancel()

	eventSender := newPkScriptRPCEventSender(
		stream, s.quit, pkScriptRPCSendTimeout,
	)
	defer eventSender.stop()

	err = eventSender.send(&PkScriptEvent{
		Event: &PkScriptEvent_Ack{
			Ack: rpcPkScriptRegisterAck(),
		},
	})
	if err != nil {
		return err
	}

	ackChan := make(chan *PkScriptMutationAck, 10)
	errChan := make(chan error, 1)

	go s.recvPkScriptMutations(stream, reg, ackChan, errChan)

	toRPCScan := rpcPkScriptHistoricalScan
	scanComplete :=
		chainntnfs.PkScriptNotificationHistoricalScanComplete
	for {
		select {
		case ntfn, ok := <-reg.Notifications:
			if !ok {
				return pkScriptNotificationStreamClosedErr(reg)
			}

			if ntfn.Type == scanComplete {

				scan := toRPCScan(ntfn.HistoricalScan)
				err := eventSender.send(&PkScriptEvent{
					Event: &PkScriptEvent_HistoricalScan{
						HistoricalScan: scan,
					},
				})
				if err != nil {
					return err
				}

				continue
			}

			rpcNtfn, err := rpcPkScriptNotification(ntfn)
			if err != nil {
				return err
			}

			err = eventSender.send(&PkScriptEvent{
				Event: &PkScriptEvent_Notification{
					Notification: rpcNtfn,
				},
			})
			if err != nil {
				return err
			}

		case ack := <-ackChan:
			err := eventSender.send(&PkScriptEvent{
				Event: &PkScriptEvent_Ack{Ack: ack},
			})
			if err != nil {
				return err
			}

		case err := <-errChan:
			return err

		case <-stream.Context().Done():
			if errors.Is(stream.Context().Err(), context.Canceled) {
				return nil
			}
			return stream.Context().Err()

		case <-s.quit:
			return ErrChainNotifierServerShuttingDown
		}
	}
}

// recvPkScriptMutations receives add/remove requests from a pkScript stream and
// forwards mutation acknowledgments or terminal errors to the main send loop.
func (s *Server) recvPkScriptMutations(
	stream ChainNotifier_RegisterPkScriptNtfnServer,
	reg *chainntnfs.PkScriptNotificationRegistration,
	ackChan chan<- *PkScriptMutationAck,
	errChan chan<- error) {

	for {
		req, err := stream.Recv()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				sendPkScriptStreamErr(errChan, err)
			}

			return
		}

		switch update := req.Request.(type) {
		case *PkScriptRequest_Register:
			err := duplicatePkScriptRegisterErr()
			sendPkScriptStreamErr(errChan, err)

			return

		case *PkScriptRequest_Add:
			result, err := addPkScriptsFromRPC(reg, update.Add)
			if err != nil {
				sendPkScriptStreamErr(errChan, err)

				return
			}

			if !sendPkScriptAddAck(
				stream.Context(), s.quit,
				ackChan, result,
			) {
				return
			}

		case *PkScriptRequest_Remove:
			err := removePkScriptsFromRPC(reg, update.Remove)
			if err != nil {
				sendPkScriptStreamErr(errChan, err)

				return
			}

			if !sendPkScriptRemoveAck(
				stream.Context(), s.quit, ackChan,
			) {
				return
			}

		default:
			err := invalidPkScriptRequestErr()
			sendPkScriptStreamErr(errChan, err)

			return
		}
	}
}

// sendPkScriptStreamErr forwards a terminal stream error without blocking if
// another terminal error has already been reported.
func sendPkScriptStreamErr(errChan chan<- error, err error) {
	select {
	case errChan <- err:
	default:
	}
}

// addPkScriptsFromRPC applies an RPC add mutation to the notifier.
func addPkScriptsFromRPC(reg *chainntnfs.PkScriptNotificationRegistration,
	add *AddPkScriptRequest) (*chainntnfs.PkScriptAddResult, error) {

	opts, err := rpcAddPkScriptOptions(add)
	if err != nil {
		return nil, err
	}

	return reg.AddPkScripts(add.PkScripts, opts...)
}

// removePkScriptsFromRPC applies an RPC remove mutation to the notifier.
func removePkScriptsFromRPC(reg *chainntnfs.PkScriptNotificationRegistration,
	remove *RemovePkScriptRequest) error {

	if remove == nil {
		return status.Error(
			codes.InvalidArgument, "remove request must be set",
		)
	}

	return reg.RemovePkScripts(remove.PkScripts)
}

func rpcPkScriptRegisterAck() *PkScriptMutationAck {
	return rpcPkScriptAck(
		PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_REGISTER,
		nil,
	)
}

func duplicatePkScriptRegisterErr() error {
	return status.Error(
		codes.InvalidArgument,
		"pkScript register action may only be sent once",
	)
}

func invalidPkScriptRequestErr() error {
	return status.Error(
		codes.InvalidArgument,
		"pkScript request must contain register, add, or remove",
	)
}

func sendPkScriptAddAck(
	ctx context.Context, quit <-chan struct{},
	ackChan chan<- *PkScriptMutationAck,
	result *chainntnfs.PkScriptAddResult,
) bool {

	return sendPkScriptMutationAck(
		ctx, quit, ackChan,
		PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_ADD,
		result,
	)
}

func sendPkScriptRemoveAck(
	ctx context.Context, quit <-chan struct{},
	ackChan chan<- *PkScriptMutationAck,
) bool {

	return sendPkScriptMutationAck(
		ctx, quit, ackChan,
		PkScriptMutationAction_PK_SCRIPT_MUTATION_ACTION_REMOVE,
		nil,
	)
}

// sendPkScriptMutationAck forwards a mutation acknowledgment unless the stream
// or server has exited.
func sendPkScriptMutationAck(
	ctx context.Context, quit <-chan struct{},
	ackChan chan<- *PkScriptMutationAck,
	action PkScriptMutationAction,
	result *chainntnfs.PkScriptAddResult,
) bool {

	select {
	case ackChan <- rpcPkScriptAck(action, result):
		return true
	case <-ctx.Done():
		return false
	case <-quit:
		return false
	}
}

// RegisterBlockEpochNtfn is a synchronous response-streaming RPC that registers
// an intent for a client to be notified of blocks in the chain. The stream will
// return a hash and height tuple of a block for each new/stale block in the
// chain. It is the client's responsibility to determine whether the tuple
// returned is for a new or stale block in the chain.
//
// A client can also request a historical backlog of blocks from a particular
// point. This allows clients to be idempotent by ensuring that they do not
// missing processing a single block within the chain.
//
// NOTE: This is part of the chainrpc.ChainNotifierServer interface.
func (s *Server) RegisterBlockEpochNtfn(in *BlockEpoch,
	epochStream ChainNotifier_RegisterBlockEpochNtfnServer) error {

	if !s.cfg.ChainNotifier.Started() {
		return ErrChainNotifierServerNotActive
	}

	// We'll start by reconstructing the RPC request into what the
	// underlying ChainNotifier expects.
	var hash chainhash.Hash
	copy(hash[:], in.Hash)

	// If the request isn't for a zero hash and a zero height, then we
	// should deliver a backlog of notifications from the given block
	// (hash/height tuple) until tip, and continue delivering epochs for
	// new blocks.
	var blockEpoch *chainntnfs.BlockEpoch
	if hash != chainntnfs.ZeroHash && in.Height != 0 {
		blockEpoch = &chainntnfs.BlockEpoch{
			Hash:   &hash,
			Height: int32(in.Height),
		}
	}

	epochEvent, err := s.cfg.ChainNotifier.RegisterBlockEpochNtfn(blockEpoch)
	if err != nil {
		return err
	}
	defer epochEvent.Cancel()

	for {
		select {
		// A notification for a block has been received. This block can
		// either be a new block or stale.
		case blockEpoch, ok := <-epochEvent.Epochs:
			if !ok {
				return chainntnfs.ErrChainNotifierShuttingDown
			}

			epoch := &BlockEpoch{
				Hash:   blockEpoch.Hash[:],
				Height: uint32(blockEpoch.Height),
			}
			if err := epochStream.Send(epoch); err != nil {
				return err
			}

		// The response stream's context for whatever reason has been
		// closed. If context is closed by an exceeded deadline we will
		// return an error.
		case <-epochStream.Context().Done():
			if errors.Is(epochStream.Context().Err(), context.Canceled) {
				return nil
			}
			return epochStream.Context().Err()

		// The server has been requested to shut down.
		case <-s.quit:
			return ErrChainNotifierServerShuttingDown
		}
	}
}
