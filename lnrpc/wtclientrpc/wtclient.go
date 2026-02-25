package wtclientrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognizes it as the name of our
	// RPC service.
	subServerName = "WatchtowerClientRPC"
)

var (
	// macPermissions maps RPC calls to the permissions they require.
	//
	// TODO(wilmer): create tower macaroon?
	macPermissions = map[string][]bakery.Op{
		"/wtclientrpc.WatchtowerClient/AddTower": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/wtclientrpc.WatchtowerClient/RemoveTower": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/wtclientrpc.WatchtowerClient/DeactivateTower": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/wtclientrpc.WatchtowerClient/TerminateSession": {{
			Entity: "offchain",
			Action: "write",
		}},
		"/wtclientrpc.WatchtowerClient/ListTowers": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/wtclientrpc.WatchtowerClient/GetTowerInfo": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/wtclientrpc.WatchtowerClient/Stats": {{
			Entity: "offchain",
			Action: "read",
		}},
		"/wtclientrpc.WatchtowerClient/Policy": {{
			Entity: "offchain",
			Action: "read",
		}},
	}

	// ErrWtclientNotActive signals that RPC calls cannot be processed
	// because the watchtower client is not active.
	ErrWtclientNotActive = errors.New("watchtower client not active")
)

// ServerShell is a shell struct holding a reference to the actual sub-server.
// It is used to register the gRPC sub-server with the root server before we
// have the necessary dependencies to populate the actual sub-server.
type ServerShell struct {
	WatchtowerClientServer
}

// WatchtowerClient is the RPC server we'll use to interact with the backing
// active watchtower client.
//
// TODO(wilmer): better name?
type WatchtowerClient struct {
	// Required by the grpc-gateway/v2 library for forward compatibility.
	UnimplementedWatchtowerClientServer

	cfg Config
}

// A compile time check to ensure that WatchtowerClient fully implements the
// WatchtowerClientWatchtowerClient gRPC service.
var _ WatchtowerClientServer = (*WatchtowerClient)(nil)

// New returns a new instance of the wtclientrpc WatchtowerClient sub-server.
// We also return the set of permissions for the macaroons that we may create
// within this method. If the macaroons we need aren't found in the filepath,
// then we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*WatchtowerClient, lnrpc.MacaroonPerms, error) {
	return &WatchtowerClient{cfg: *cfg}, macPermissions, nil
}

// Start launches any helper goroutines required for the WatchtowerClient to
// function.
//
// NOTE: This is part of the lnrpc.SubWatchtowerClient interface.
func (c *WatchtowerClient) Start() error {
	return nil
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (c *WatchtowerClient) Stop() error {
	return nil
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (c *WatchtowerClient) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a sub
// RPC server to register itself with the main gRPC root server. Until this is
// called, each sub-server won't be able to have requests routed towards it.
//
// NOTE: This is part of the lnrpc.GrpcHandler interface.
func (r *ServerShell) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterWatchtowerClientServer(grpcServer, r)

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
	err := RegisterWatchtowerClientHandlerFromEndpoint(ctx, mux, dest, opts)
	if err != nil {
		return err
	}

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

	r.WatchtowerClientServer = subServer
	return subServer, macPermissions, nil
}

// isActive returns nil if the watchtower client is initialized so that we can
// process RPC requests.
func (c *WatchtowerClient) isActive() error {
	if c.cfg.Active {
		return nil
	}
	return ErrWtclientNotActive
}

// AddTower adds a new watchtower reachable at the given address and considers
// it for new sessions. If the watchtower already exists, then any new addresses
// included will be considered when dialing it for session negotiations and
// backups.
func (c *WatchtowerClient) AddTower(ctx context.Context,
	req *AddTowerRequest) (*AddTowerResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.Pubkey)
	if err != nil {
		return nil, err
	}
	addr, err := lncfg.ParseAddressString(
		req.Address, strconv.Itoa(watchtower.DefaultPeerPort),
		c.cfg.Resolver,
	)
	if err != nil {
		return nil, fmt.Errorf("invalid address %v: %w", req.Address,
			err)
	}

	towerAddr := &lnwire.NetAddress{
		IdentityKey: pubKey,
		Address:     addr,
	}

	if err := c.cfg.ClientMgr.AddTower(towerAddr); err != nil {
		return nil, err
	}

	return &AddTowerResponse{}, nil
}

// RemoveTower removes a watchtower from being considered for future session
// negotiations and from being used for any subsequent backups until it's added
// again. If an address is provided, then this RPC only serves as a way of
// removing the address from the watchtower instead.
func (c *WatchtowerClient) RemoveTower(ctx context.Context,
	req *RemoveTowerRequest) (*RemoveTowerResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.Pubkey)
	if err != nil {
		return nil, err
	}

	var addr net.Addr
	if req.Address != "" {
		addr, err = lncfg.ParseAddressString(
			req.Address, strconv.Itoa(watchtower.DefaultPeerPort),
			c.cfg.Resolver,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to parse tower "+
				"address %v: %v", req.Address, err)
		}
	}

	err = c.cfg.ClientMgr.RemoveTower(pubKey, addr)
	if err != nil {
		return nil, err
	}

	return &RemoveTowerResponse{}, nil
}

// DeactivateTower sets the given tower's status to inactive so that it is not
// considered for session negotiation. Its sessions will also not be used while
// the tower is inactive.
func (c *WatchtowerClient) DeactivateTower(_ context.Context,
	req *DeactivateTowerRequest) (*DeactivateTowerResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.Pubkey)
	if err != nil {
		return nil, err
	}

	err = c.cfg.ClientMgr.DeactivateTower(pubKey)
	if err != nil {
		return nil, err
	}

	return &DeactivateTowerResponse{
		Status: fmt.Sprintf("Successful deactivation of tower: %x",
			req.Pubkey),
	}, nil
}

// TerminateSession terminates the given session and marks it as terminal so
// that it is never used again.
func (c *WatchtowerClient) TerminateSession(_ context.Context,
	req *TerminateSessionRequest) (*TerminateSessionResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.SessionId)
	if err != nil {
		return nil, err
	}

	sessionID := wtdb.NewSessionIDFromPubKey(pubKey)

	err = c.cfg.ClientMgr.TerminateSession(sessionID)
	if err != nil {
		return nil, err
	}

	return &TerminateSessionResponse{
		Status: fmt.Sprintf("Successful termination of session: %s",
			sessionID),
	}, nil
}

// ListTowers returns the list of watchtowers registered with the client.
func (c *WatchtowerClient) ListTowers(ctx context.Context,
	req *ListTowersRequest) (*ListTowersResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	opts, ackCounts, committedUpdateCounts := constructFunctionalOptions(
		req.IncludeSessions, req.ExcludeExhaustedSessions,
	)

	towersPerBlobType, err := c.cfg.ClientMgr.RegisteredTowers(opts...)
	if err != nil {
		return nil, err
	}

	// Collect all the legacy client towers. If it has any of the same
	// towers that the anchors client has, then just add the session info
	// for the legacy client to the existing tower.
	rpcTowers := make(map[wtdb.TowerID]*Tower)
	for blobType, towers := range towersPerBlobType {
		policyType, err := blobTypeToPolicyType(blobType)
		if err != nil {
			return nil, err
		}

		for _, tower := range towers {
			rpcTower := marshallTower(
				tower, policyType, req.IncludeSessions,
				ackCounts, committedUpdateCounts,
			)

			t, ok := rpcTowers[tower.ID]
			if !ok {
				rpcTowers[tower.ID] = rpcTower
				continue
			}

			t.SessionInfo = append(
				t.SessionInfo, rpcTower.SessionInfo...,
			)
			t.Sessions = append(
				t.Sessions, rpcTower.Sessions...,
			)
		}
	}

	towers := make([]*Tower, 0, len(rpcTowers))
	for _, tower := range rpcTowers {
		towers = append(towers, tower)
	}

	return &ListTowersResponse{Towers: towers}, nil
}

// GetTowerInfo retrieves information for a registered watchtower.
func (c *WatchtowerClient) GetTowerInfo(ctx context.Context,
	req *GetTowerInfoRequest) (*Tower, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	pubKey, err := btcec.ParsePubKey(req.Pubkey)
	if err != nil {
		return nil, err
	}

	opts, ackCounts, committedUpdateCounts := constructFunctionalOptions(
		req.IncludeSessions, req.ExcludeExhaustedSessions,
	)

	towersPerBlobType, err := c.cfg.ClientMgr.LookupTower(pubKey, opts...)
	if err != nil {
		return nil, err
	}

	var resTower *Tower
	for blobType, tower := range towersPerBlobType {
		policyType, err := blobTypeToPolicyType(blobType)
		if err != nil {
			return nil, err
		}

		rpcTower := marshallTower(
			tower, policyType, req.IncludeSessions,
			ackCounts, committedUpdateCounts,
		)

		if resTower == nil {
			resTower = rpcTower
			continue
		}

		if !bytes.Equal(rpcTower.Pubkey, resTower.Pubkey) {
			return nil, fmt.Errorf("tower clients returned " +
				"inconsistent results for the given tower")
		}

		resTower.SessionInfo = append(
			resTower.SessionInfo, rpcTower.SessionInfo...,
		)
		resTower.Sessions = append(
			resTower.Sessions, rpcTower.Sessions...,
		)
	}

	return resTower, nil
}

// constructFunctionalOptions is a helper function that constructs a list of
// functional options to be used when fetching a tower from the DB. It also
// returns a map of acked-update counts and one for un-acked-update counts that
// will be populated once the db call has been made.
func constructFunctionalOptions(includeSessions,
	excludeExhaustedSessions bool) ([]wtdb.ClientSessionListOption,
	map[wtdb.SessionID]uint16, map[wtdb.SessionID]uint16) {

	var (
		opts                  []wtdb.ClientSessionListOption
		committedUpdateCounts = make(map[wtdb.SessionID]uint16)
		ackCounts             = make(map[wtdb.SessionID]uint16)
	)
	if !includeSessions {
		return opts, ackCounts, committedUpdateCounts
	}

	perNumRogueUpdates := func(s *wtdb.ClientSession, numUpdates uint16) {
		ackCounts[s.ID] += numUpdates
	}

	perNumAckedUpdates := func(s *wtdb.ClientSession, id lnwire.ChannelID,
		numUpdates uint16) {

		ackCounts[s.ID] += numUpdates
	}

	perCommittedUpdate := func(s *wtdb.ClientSession,
		u *wtdb.CommittedUpdate) {

		committedUpdateCounts[s.ID]++
	}

	opts = []wtdb.ClientSessionListOption{
		wtdb.WithPerNumAckedUpdates(perNumAckedUpdates),
		wtdb.WithPerCommittedUpdate(perCommittedUpdate),
		wtdb.WithPerRogueUpdateCount(perNumRogueUpdates),
	}

	if excludeExhaustedSessions {
		opts = append(opts, wtdb.WithPostEvalFilterFn(
			wtclient.ExhaustedSessionFilter(),
		))
	}

	return opts, ackCounts, committedUpdateCounts
}

// Stats returns the in-memory statistics of the client since startup.
func (c *WatchtowerClient) Stats(_ context.Context,
	_ *StatsRequest) (*StatsResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	stats := c.cfg.ClientMgr.Stats()

	return &StatsResponse{
		NumBackups:           uint32(stats.NumTasksAccepted),
		NumFailedBackups:     uint32(stats.NumTasksIneligible),
		NumPendingBackups:    uint32(stats.NumTasksPending),
		NumSessionsAcquired:  uint32(stats.NumSessionsAcquired),
		NumSessionsExhausted: uint32(stats.NumSessionsExhausted),
	}, nil
}

// Policy returns the active watchtower client policy configuration.
func (c *WatchtowerClient) Policy(ctx context.Context,
	req *PolicyRequest) (*PolicyResponse, error) {

	if err := c.isActive(); err != nil {
		return nil, err
	}

	blobType, err := policyTypeToBlobType(req.PolicyType)
	if err != nil {
		return nil, err
	}

	policy, err := c.cfg.ClientMgr.Policy(blobType)
	if err != nil {
		return nil, err
	}

	return &PolicyResponse{
		MaxUpdates:       uint32(policy.MaxUpdates),
		SweepSatPerVbyte: uint32(policy.SweepFeeRate.FeePerVByte()),

		// Deprecated field.
		SweepSatPerByte: uint32(policy.SweepFeeRate.FeePerVByte()),
	}, nil
}

// marshallTower converts a client registered watchtower into its corresponding
// RPC type.
func marshallTower(tower *wtclient.RegisteredTower, policyType PolicyType,
	includeSessions bool, ackCounts map[wtdb.SessionID]uint16,
	pendingCounts map[wtdb.SessionID]uint16) *Tower {

	rpcAddrs := make([]string, 0, len(tower.Addresses))
	for _, addr := range tower.Addresses {
		rpcAddrs = append(rpcAddrs, addr.String())
	}

	var rpcSessions []*TowerSession
	if includeSessions {
		// To ensure that the output order is deterministic for a given
		// set of sessions, we put the sessions into a slice and order
		// them based on session ID.
		sessions := make([]*wtdb.ClientSession, 0, len(tower.Sessions))
		for _, session := range tower.Sessions {
			sessions = append(sessions, session)
		}

		sort.Slice(sessions, func(i, j int) bool {
			id1 := sessions[i].ID
			id2 := sessions[j].ID

			return binary.BigEndian.Uint64(id1[:]) <
				binary.BigEndian.Uint64(id2[:])
		})

		rpcSessions = make([]*TowerSession, 0, len(tower.Sessions))
		for _, session := range sessions {
			satPerVByte := session.Policy.SweepFeeRate.FeePerVByte()
			rpcSessions = append(rpcSessions, &TowerSession{
				Id:                session.ID[:],
				NumBackups:        uint32(ackCounts[session.ID]),
				NumPendingBackups: uint32(pendingCounts[session.ID]),
				MaxBackups:        uint32(session.Policy.MaxUpdates),
				SweepSatPerVbyte:  uint32(satPerVByte),

				// Deprecated field.
				SweepSatPerByte: uint32(satPerVByte),
			})
		}
	}

	rpcTower := &Tower{
		Pubkey:    tower.IdentityKey.SerializeCompressed(),
		Addresses: rpcAddrs,
		SessionInfo: []*TowerSessionInfo{{
			PolicyType:             policyType,
			ActiveSessionCandidate: tower.ActiveSessionCandidate,
			NumSessions:            uint32(len(tower.Sessions)),
			Sessions:               rpcSessions,
		}},
		// The below fields are populated for backwards compatibility
		// but will be removed in a future commit when the proto fields
		// are removed.
		ActiveSessionCandidate: tower.ActiveSessionCandidate,
		NumSessions:            uint32(len(tower.Sessions)),
		Sessions:               rpcSessions,
	}

	return rpcTower
}

func blobTypeToPolicyType(t blob.Type) (PolicyType, error) {
	switch t {
	case blob.TypeAltruistTaprootCommit:
		return PolicyType_TAPROOT, nil

	case blob.TypeAltruistAnchorCommit:
		return PolicyType_ANCHOR, nil

	case blob.TypeAltruistCommit:
		return PolicyType_LEGACY, nil

	default:
		return 0, fmt.Errorf("unknown blob type: %s", t)
	}
}

func policyTypeToBlobType(t PolicyType) (blob.Type, error) {
	switch t {
	case PolicyType_TAPROOT:
		return blob.TypeAltruistTaprootCommit, nil

	case PolicyType_ANCHOR:
		return blob.TypeAltruistAnchorCommit, nil

	case PolicyType_LEGACY:
		return blob.TypeAltruistCommit, nil

	default:
		return 0, fmt.Errorf("unknown policy type: %s", t)
	}
}
