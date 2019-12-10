package sweep

import (
	"github.com/btcsuite/btcd/wire"
)

// Wallet contains all wallet related functionality required by sweeper.
type Wallet interface {
	// PublishTransaction performs cursory validation (dust checks, etc) and
	// broadcasts the passed transaction to the Bitcoin network.
	PublishTransaction(tx *wire.MsgTx) error
}
