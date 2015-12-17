package wallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/revocation"

	"github.com/btcsuite/btcwallet/walletdb"
)

type LightningChannel struct {
	shachan *revocation.HyperShaChain
	wallet  *LightningWallet

	channelEvents *chainntnfs.ChainNotifier
// P2SHify...
func P2SHify(scriptBytes []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_HASH160)
	bldr.AddData(btcutil.Hash160(scriptBytes))
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

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
