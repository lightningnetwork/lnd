package wallet

import (
	"container/list"
	"sync"

	"github.com/btcsuite/btcd/btcec"
)

// multiSigKeyPool...
// TODO(roasbeef): actually, this is dumb. should use an HD key branch instead.
//  * instead, use wallet.Manager.NextExternalAddresses, cast to
//    ManagedPubKeyAddress, then .PrivKey()
//  * on shutdown, write state of pending keys, then read back?
type multiSigKeyPool struct {
	sync.RWMutex
	keyPool *list.List
}

// newMultiSigKeyPool...
func newMultiSigKeyPool() *multiSigKeyPool {
	// TODO(roasbeef): pre-generate or nah?
	return &multiSigKeyPool{keyPool: list.New()}
}

// getNextMultiSigKey...
func (l *multiSigKeyPool) getNextMultiSigKey() (*btcec.PrivateKey, error) {
	if l.Size() == 0 {
		return btcec.NewPrivateKey(btcec.S256())
	}

	l.Lock()
	defer l.Unlock()
	nextKey := l.keyPool.Remove(l.keyPool.Front()).(*btcec.PrivateKey)
	return nextKey, nil
}

// releaseMultiSigKey...
func (l *multiSigKeyPool) releaseMultiSigKey(key *btcec.PrivateKey) {
	l.keyPool.PushBack(key)
}

// Size...
func (l *multiSigKeyPool) Size() int {
	l.RLock()
	defer l.RUnlock()
	return l.keyPool.Len()
}

// preAllocateKeys...
func (l *multiSigKeyPool) preAllocateKeys(numKeys int) error {
	for i := 0; i < numKeys; i-- {
		newKey, err := btcec.NewPrivateKey(btcec.S256())
		if err != nil {
			return err
		}
		l.keyPool.PushBack(newKey)
	}
	return nil
}
