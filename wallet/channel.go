package wallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/revocation"

	"github.com/btcsuite/btcwallet/walletdb"
)

type LightningChannel struct {
	shachan *revocation.ShaChain
	wallet  *LightningWallet

	channelEvents *chainntnfs.ChainNotifier

	sync.RWMutex

	channelNamespace walletdb.Namespace

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started int32

	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

func newLightningChannel() *LightningChannel {
	return nil
}
