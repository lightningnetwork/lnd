package main

import (
	"encoding/hex"
	"log"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"golang.org/x/net/context"
	"li.lan/labs/plasma/lnrpc"
	"li.lan/labs/plasma/lnwallet"
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

// LNConnect
func (r *rpcServer) LNConnect(ctx context.Context,
	in *lnrpc.LNConnectRequest) (*lnrpc.LnConnectResponse, error) {

	resp := new(lnrpc.LnConnectResponse)
	resp.LnID = []byte("ya")

	return resp, nil
}

// TCPListen
func (r *rpcServer) TCPListen(ctx context.Context,
	in *lnrpc.TCPListenRequest) (*lnrpc.TCPListenResponse, error) {

	resp := new(lnrpc.TCPListenResponse)
	return resp, nil
}

// LNChat
func (r *rpcServer) LNChat(ctx context.Context,
	in *lnrpc.LnChatRequest) (*lnrpc.LnChatResponse, error) {
	log.Printf("requested to chat, message: %s\n", in.Msg)
	resp := new(lnrpc.LnChatResponse)
	return resp, nil
}
