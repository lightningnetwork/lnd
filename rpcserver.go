package main

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"strings"

	"li.lan/labs/testnet-L/chaincfg"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/lndc"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"golang.org/x/net/context"
)

var (
	defaultAccount uint32 = waddrmgr.DefaultAccountNum
)

// rpcServer...
type rpcServer struct { // doesn't count as globals I think
	lnwallet *lnwallet.LightningWallet // interface to the bitcoin network
	CnMap    map[[16]byte]net.Conn     //interface to the lightning network
	OmniChan chan []byte               // channel for all incoming messages from LN nodes.
	// can split the OmniChan up if that is helpful.  So far 1 seems OK.
}

type LNAdr struct {
	LNId   [16]byte // redundant because adr contains it
	Adr    btcutil.Address
	PubKey *btcec.PublicKey

	Name        string
	Host        string
	Endorsement []byte
}

func (l *LNAdr) ParseFromString(s string) error {
	var err error
	if len(s) == 0 {
		return fmt.Errorf("LNid ParseFromString: null string")
	}
	if l == nil { // make a LNId if it doesn't exist
		return fmt.Errorf("null id, initialize first")
	}

	ss := strings.Split(s, "@")
	ident := ss[0]
	if len(ss) > 1 { // redundant? no
		l.Host = ss[1]
	}
	if len(ident) > 65 && len(ident) < 69 { // could be pubkey
		pubkeyBytes, err := hex.DecodeString(ident)
		if err != nil {
			return err
		}
		l.PubKey, err = btcec.ParsePubKey(pubkeyBytes, btcec.S256())
		if err != nil {
			return err
		}

		// got pubey, populate address from pubkey
		l.Adr, err = btcutil.NewAddressPubKeyHash(btcutil.Hash160(
			l.PubKey.SerializeCompressed()), &chaincfg.TestNet3Params)
		if err != nil {
			return err
		}
	} else if len(ident) > 33 && len(ident) < 37 { // could be pkh
		l.Adr, err = btcutil.DecodeAddress(
			ident, &chaincfg.TestNet3Params)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("invalid address %s", ident)
	}

	// check for nonstandard port
	//	if strings.Count(lid.Host, ":") == 1 {
	//	}
	//don't for now... stdlib should do this.  ipv6 and stuff...

	//	populate LNId from address
	copy(l.LNId[:], l.Adr.ScriptAddress())
	return nil
}

var _ lnrpc.LightningServer = (*rpcServer)(nil)

// newRpcServer...
func newRpcServer(wallet *lnwallet.LightningWallet) *rpcServer {
	return &rpcServer{wallet,
		make(map[[16]byte]net.Conn), // initialize with empty CnMap
		make(chan []byte)}           // init OmniChan (size 1 ok...?)
}

// Stop...
func (r *rpcServer) Stop() error {
	return nil
}

// getPriv gets the identity private key out of the wallet DB
func getPriv(l *lnwallet.LightningWallet) (*btcec.PrivateKey, error) {
	adr, err := l.ChannelDB.GetIdAdr()
	if err != nil {
		return nil, err
	}
	fmt.Printf("got ID address: %s\n", adr.String())
	adr2, err := l.Manager.Address(adr)
	if err != nil {
		return nil, err
	}
	priv, err := adr2.(waddrmgr.ManagedPubKeyAddress).PrivKey()
	if err != nil {
		return nil, err
	}
	fmt.Printf("got privkey %x\n", priv.Serialize()) // may want to remove this :)
	return priv, nil
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

	var err error

	if len(in.IdAtHost) == 0 {
		return nil, fmt.Errorf("need: lnc pubkeyhash@hostname")
	}
	var newNode LNAdr

	err = newNode.ParseFromString(in.IdAtHost)
	if err != nil {
		return nil, err
	}
	if _, ok := r.CnMap[newNode.LNId]; ok {
		return nil, fmt.Errorf("Already connected to %x", newNode.LNId)
	}
	if newNode.Host == "" { // do PBX connect; leave for now
		return nil, fmt.Errorf("no hostname")
	}
	priv, err := getPriv(r.lnwallet)
	if err != nil {
		return nil, err
	}

	// dial TCP
	newConn := new(lndc.LNDConn)
	newConn.Cn, err = net.Dial("tcp", newNode.Host+":"+"2448")
	if err != nil {
		return nil, err
	}
	// TODO differentiate; right now only uses PKH
	if newNode.PubKey != nil { // have pubkey, use that
		err = newConn.Open(priv, newNode.Adr.ScriptAddress())
		if err != nil {
			return nil, err
		}
	} else { // only have address (pubkey hash), use that
		err = newConn.Open(priv, newNode.Adr.ScriptAddress())
		if err != nil {
			return nil, err
		}
	}

	idslice := lndc.H160(newConn.RemotePub.SerializeCompressed())
	var newId [16]byte
	copy(newId[:], idslice[:16])
	r.CnMap[newId] = newConn
	log.Printf("added %x to map\n", newId)

	go LNDCReceiver(newConn, newId, r)

	resp := new(lnrpc.LnConnectResponse)
	resp.LnID = newId[:]
	return resp, nil
}

// TCPListen
func (r *rpcServer) TCPListen(ctx context.Context,
	in *lnrpc.TCPListenRequest) (*lnrpc.TCPListenResponse, error) {
	// LnListen listens on the default port for incoming connections
	//ignore args and launch listener goroutine
	priv, err := getPriv(r.lnwallet)
	if err != nil {
		return nil, err
	}

	go TCPListener(priv, r)

	resp := new(lnrpc.TCPListenResponse)
	return resp, nil
}

// LNChat
func (r *rpcServer) LNChat(ctx context.Context,
	in *lnrpc.LnChatRequest) (*lnrpc.LnChatResponse, error) {
	log.Printf("requested to chat, message: %s\n", in.Msg)

	var dest [16]byte
	if len(in.DestID) != 16 {
		return nil, fmt.Errorf("Expect 16 byte destination Id, got %d byte",
			len(in.DestID))
	}
	copy(dest[:], in.DestID)
	if len(in.Msg) == 0 {
		return nil, fmt.Errorf("you have to say something")
	}
	if len(r.CnMap) == 0 { // This check is redundant.  May still help though.
		return nil, fmt.Errorf("Not connected to anyone")
	}
	if _, ok := r.CnMap[dest]; !ok {
		return nil, fmt.Errorf("dest %x not connected", dest)
	}

	msg := append([]byte{lnwire.MSGID_TEXTCHAT}, []byte(in.Msg)...)

	_, err := r.CnMap[dest].Write(msg)
	if err != nil {
		return nil, err
	}

	resp := new(lnrpc.LnChatResponse)
	return resp, nil
}

func TCPListener(priv *btcec.PrivateKey, r *rpcServer) {
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
		newConn, err := InitIncomingConn(priv, con)
		if err != nil {
			fmt.Printf("InitConn error: %s\n", err.Error())
			continue
		}
		idslice := lndc.H160(newConn.RemotePub.SerializeCompressed())
		var newId [16]byte
		copy(newId[:], idslice[:16])
		r.CnMap[newId] = newConn
		fmt.Printf("added %x to map\n", newId)

		go LNDCReceiver(newConn, newId, r)
	}
}

func InitIncomingConn(priv *btcec.PrivateKey, con net.Conn) (*lndc.LNDConn, error) {
	LNcon := new(lndc.LNDConn)
	LNcon.Cn = con
	err := LNcon.Setup(priv)
	if err != nil {
		return LNcon, err
	}
	fmt.Printf("Got connection from %s authed with pubkey %x",
		LNcon.Cn.RemoteAddr().String(), LNcon.RemotePub.SerializeCompressed())
	return LNcon, nil

}
