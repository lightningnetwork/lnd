// +build !rpctest

package main

import (
	"testing"
	"github.com/lightningnetwork/lnd/lnrpc"
	"io/ioutil"
	"github.com/roasbeef/btcd/btcec"
	"github.com/btcsuite/btclog"
	"strings"
	"sync/atomic"
)

func init() {
	rpcsLog = btclog.Disabled
}

func TestNewAddress(t *testing.T) {

	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		alicePrivKeyBytes[:])

	testnode, err := createTestFundingManager(t, alicePrivKey, aliceTestDir)

	r := &rpcServer{}
	r.server = &server{}
	r.server.cc = &chainControl{}
	r.server.cc.wallet= testnode.fundingMgr.cfg.Wallet

	//r.server.cc.wallet.NewAddress = func NewAddress(addrType AddressType, change bool) (btcutil.Address, error)


	req := &lnrpc.NewAddressRequest{
		Type: lnrpc.NewAddressRequest_WITNESS_PUBKEY_HASH,
	}

	_ , err = r.NewAddress(nil,req)
	if err != nil {
		t.Fatalf("Failed to get WITNESS_PUBKEY_HASH address")
	}

	req = &lnrpc.NewAddressRequest{
		Type: lnrpc.NewAddressRequest_NESTED_PUBKEY_HASH,
	}

	_ , err = r.NewAddress(nil,req)
	if err != nil {
		t.Fatalf("Failed to get NESTED_PUBKEY_HASH address")
	}


}


func TestWalletBalance(t *testing.T) {



	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		alicePrivKeyBytes[:])

	testnode, err := createTestFundingManager(t, alicePrivKey, aliceTestDir)

	r := &rpcServer{}
	r.server = &server{}
	r.server.cc = &chainControl{}
	r.server.cc.wallet= testnode.fundingMgr.cfg.Wallet

	registeredChains.RegisterChain(bitcoinChain,r.server.cc)

	//r.server.cc.wallet.NewAddress = func NewAddress(addrType AddressType, change bool) (btcutil.Address, error)


	req := &lnrpc.WalletBalanceRequest{
	}

	_ , err = r.WalletBalance(nil,req)
	if err != nil {
		t.Fatalf("Failed to get WalletBalance")
	}

}

func TestGetTransactions(t *testing.T) {

	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		alicePrivKeyBytes[:])

	testnode, err := createTestFundingManager(t, alicePrivKey, aliceTestDir)

	r := &rpcServer{}
	r.server = &server{}
	r.server.cc = &chainControl{}
	r.server.cc.wallet= testnode.fundingMgr.cfg.Wallet

	registeredChains.RegisterChain(bitcoinChain,r.server.cc)


	req := &lnrpc.GetTransactionsRequest{
	}

	_ , err = r.GetTransactions(nil,req)
	if err != nil {
		t.Fatalf("Failed to get GetTransactions")
	}

}

func TestDisconnectPeer(t *testing.T) {

	aliceTestDir, err := ioutil.TempDir("", "alicelnwallet")
	if err != nil {
		t.Fatalf("unable to create temp directory: %v", err)
	}

	// Use hard-coded keys for Alice and Bob, the two FundingManagers that
	// we will test the interaction between.
	alicePrivKeyBytes = [32]byte{
		0xb7, 0x94, 0x38, 0x5f, 0x2d, 0x1e, 0xf7, 0xab,
		0x4d, 0x92, 0x73, 0xd1, 0x90, 0x63, 0x81, 0xb4,
		0x4f, 0x2f, 0x6f, 0x25, 0x88, 0xa3, 0xef, 0xb9,
		0x6a, 0x49, 0x18, 0x83, 0x31, 0x98, 0x47, 0x53,
	}

	alicePrivKey, alicePubKey = btcec.PrivKeyFromBytes(btcec.S256(),
		alicePrivKeyBytes[:])

	testnode, err := createTestFundingManager(t, alicePrivKey, aliceTestDir)

	r := &rpcServer{}
	r.server = &server{}
	r.server.cc = &chainControl{}
	r.server.cc.wallet= testnode.fundingMgr.cfg.Wallet

	registeredChains.RegisterChain(bitcoinChain,r.server.cc)


	req := &lnrpc.DisconnectPeerRequest{
		PubKey: 	"Invalid public key",
	}

	_ , err = r.DisconnectPeer(nil,req)
	switch  {
	case err == nil:
		t.Fatalf("got no error from DisconnectPeerRequest")
	case !strings.Contains(err.Error(),"chain backend is still syncing"):
		t.Fatalf("didn't get 'chain backend is still syncing'")
	}

	if !atomic.CompareAndSwapInt32(&r.server.started, 0, 1) {
		t.Fatalf("can't mark server started")
	}

	_ , err = r.DisconnectPeer(nil,req)
	switch  {
	case err == nil:
		t.Fatalf("got no error from DisconnectPeerRequest")
	case !strings.Contains(err.Error(),"unable to decode pubkey bytes"):
		t.Fatalf("didn't get 'unable to decode pubkey bytes'")
	}

}


