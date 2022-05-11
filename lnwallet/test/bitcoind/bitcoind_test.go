package bitcoind_test

import (
	"testing"

	lnwallettest "github.com/lightningnetwork/lnd/lnwallet/test"
)

// TestLightningWalletBitcoindZMQ tests LightningWallet powered by bitcoind,
// using its ZMQ interface, against our suite of interface tests.
func TestLightningWalletBitcoindZMQ(t *testing.T) {
	lnwallettest.TestLightningWallet(t, "bitcoind")
}

// TestLightningWalletBitcoindRPCPolling tests LightningWallet powered by
// bitcoind, using its RPC interface, against our suite of interface tests.
func TestLightningWalletBitcoindRPCPolling(t *testing.T) {
	lnwallettest.TestLightningWallet(t, "bitcoind-rpc-polling")
}
