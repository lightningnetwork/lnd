package wallet

import (
	//"fmt"
	"github.com/btcsuite/btcd/wire"
	"testing"
)

func TestRunShaChain(t *testing.T) {
	var seed wire.ShaHash
	var hash wire.ShaHash
	NewShaChainFromSeed(seed, 0, hash)
}
