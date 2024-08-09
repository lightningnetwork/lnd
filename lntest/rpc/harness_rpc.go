package rpc

import (
	"context"
	"testing"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnrpc/watchtowerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
	"github.com/lightningnetwork/lnd/lntest/wait"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

const (
	DefaultTimeout = wait.DefaultTimeout
)

// HarnessRPC wraps all lnd's RPC clients into a single struct for easier
// access.
type HarnessRPC struct {
	*testing.T

	LN               lnrpc.LightningClient
	WalletUnlocker   lnrpc.WalletUnlockerClient
	Invoice          invoicesrpc.InvoicesClient
	Signer           signrpc.SignerClient
	Router           routerrpc.RouterClient
	WalletKit        walletrpc.WalletKitClient
	Watchtower       watchtowerrpc.WatchtowerClient
	WatchtowerClient wtclientrpc.WatchtowerClientClient
	State            lnrpc.StateClient
	ChainClient      chainrpc.ChainNotifierClient
	ChainKit         chainrpc.ChainKitClient
	NeutrinoKit      neutrinorpc.NeutrinoKitClient
	Peer             peersrpc.PeersClient
	Dev              devrpc.DevClient

	// Name is the HarnessNode's name.
	Name string

	// runCtx is a context with cancel method. It's used to signal when the
	// node needs to quit, and used as the parent context when spawning
	// children contexts for RPC requests.
	runCtx context.Context //nolint:containedctx
	cancel context.CancelFunc
}

// NewHarnessRPC creates a new HarnessRPC with its own context inherted from
// the pass context.
func NewHarnessRPC(ctxt context.Context, t *testing.T, c *grpc.ClientConn,
	name string) *HarnessRPC {

	h := &HarnessRPC{
		T:                t,
		LN:               lnrpc.NewLightningClient(c),
		Invoice:          invoicesrpc.NewInvoicesClient(c),
		Router:           routerrpc.NewRouterClient(c),
		WalletKit:        walletrpc.NewWalletKitClient(c),
		WalletUnlocker:   lnrpc.NewWalletUnlockerClient(c),
		Watchtower:       watchtowerrpc.NewWatchtowerClient(c),
		WatchtowerClient: wtclientrpc.NewWatchtowerClientClient(c),
		Signer:           signrpc.NewSignerClient(c),
		State:            lnrpc.NewStateClient(c),
		ChainClient:      chainrpc.NewChainNotifierClient(c),
		ChainKit:         chainrpc.NewChainKitClient(c),
		NeutrinoKit:      neutrinorpc.NewNeutrinoKitClient(c),
		Peer:             peersrpc.NewPeersClient(c),
		Dev:              devrpc.NewDevClient(c),
		Name:             name,
	}

	// Inherit parent context.
	h.runCtx, h.cancel = context.WithCancel(ctxt)
	return h
}

// MakeOutpoint returns the outpoint of the channel's funding transaction.
func (h *HarnessRPC) MakeOutpoint(cp *lnrpc.ChannelPoint) wire.OutPoint {
	fundingTxID, err := lnrpc.GetChanPointFundingTxid(cp)
	require.NoErrorf(h, err, "failed to make chanPoint", h.Name)

	return wire.OutPoint{
		Hash:  *fundingTxID,
		Index: cp.OutputIndex,
	}
}

// NoError is a helper method to format the error message used in calling RPCs.
func (h *HarnessRPC) NoError(err error, operation string) {
	require.NoErrorf(h, err, "%s: failed to call %s", h.Name, operation)
}
