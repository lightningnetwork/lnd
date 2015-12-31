package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"

	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"golang.org/x/net/context"
	"li.lan/labs/plasma/lndc"
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
	// LnListen listens on the default port for incoming connections
	//ignore args and launch listener goroutine

	adr, err := r.lnwallet.ChannelDB.GetIdAdr()
	if err != nil {
		return nil, err
	}

	fmt.Printf("got ID address: %s\n", adr.String())
	adr2, err := r.lnwallet.Manager.Address(adr)
	if err != nil {
		return nil, err
	}
	priv, err := adr2.(waddrmgr.ManagedPubKeyAddress).PrivKey()
	if err != nil {
		return nil, err
	}
	fmt.Printf("got privkey %x\n", priv.Serialize())
	//	go TCPListener()

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

func TCPListener() {
	listener, err := net.Listen("tcp", ":"+"2448")
	if err != nil {
		fmt.Printf("TCP listen error: %s\n", err.Error())
		return
	}

	fmt.Printf("Listening on %s\n", listener.Addr().String())
	for {
		con, err := listener.Accept() // this blocks
		if err != nil {
			log.Printf("Listener error: %s\n", err.Error())
			continue
		}
		newConn, err := InitIncomingConn(con)
		if err != nil {
			fmt.Printf("InitConn error: %s\n", err.Error())
			continue
		}
		idslice := lndc.H160(newConn.RemotePub.SerializeCompressed())
		var newId [16]byte
		copy(newId[:], idslice[:16])
		//		CnMap[newId] = newConn
		fmt.Printf("added %x to map\n", newId)
		//		go LNDCReceiver(newConn, newId)
	}
}

func InitIncomingConn(con net.Conn) (*lndc.LNDConn, error) {
	LNcon := new(lndc.LNDConn)
	LNcon.Cn = con
	err := LNcon.Setup(nil)
	if err != nil {
		return LNcon, err
	}
	fmt.Printf("Got connection from %s authed with pubkey %x",
		LNcon.Cn.RemoteAddr().String(), LNcon.RemotePub.SerializeCompressed())
	return LNcon, nil

}
