package main

import (
	"encoding/hex"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"golang.org/x/net/context"
	"li.lan/labs/plasma/lnwallet"
	"li.lan/labs/plasma/rpcprotos"
)

var (
	defaultAccount uint32 = waddrmgr.DefaultAccountNum
)

// rpcServer...
type rpcServer struct {
	lnwallet *lnwallet.LightningWallet
}

var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRpcServer...
func newRpcServer(wallet *lnwallet.LightningWallet) *rpcServer {
	return &rpcServer{wallet}
}

// SendMany...
func (r *rpcServer) SendMany(ctx context.Context, in *lnrpc.SendManyRequest) (*lnrpc.SendManyResponse, error) {

	sendMap := make(map[string]btcutil.Amount)
	for addr, amt := range in.AddrToAmount {
		sendMap[addr] = btcutil.Amount(amt)
	}

	txid, err := r.lnwallet.SendPairs(sendMap, defaultAccount, 1)
	if err != nil {
		return nil, err
	}

	return &lnrpc.SendManyResponse{Txid: hex.EncodeToString(txid[:])}, nil
}

// NewAddress...
func (r *rpcServer) NewAddress(ctx context.Context, in *lnrpc.NewAddressRequest) (*lnrpc.NewAddressResponse, error) {

	r.lnwallet.KeyGenMtx.Lock()
	defer r.lnwallet.KeyGenMtx.Unlock()

	addr, err := r.lnwallet.NewAddress(defaultAccount)
	if err != nil {
		return nil, err
	}

	return &lnrpc.NewAddressResponse{Address: addr.String()}, nil
}
