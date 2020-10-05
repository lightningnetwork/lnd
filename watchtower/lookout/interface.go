package lookout

import (
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/watchtower/blob"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
)

// Service abstracts the lookout functionality, supporting the ability to start
// and stop. All communication and actions are driven via the database or chain
// events.
type Service interface {
	// Start safely starts up the Interface.
	Start() error

	// Stop safely stops the Interface.
	Stop() error
}

// BlockFetcher supports the ability to fetch blocks from the backend or
// network.
type BlockFetcher interface {
	// GetBlock fetches the block given the target block hash.
	GetBlock(*chainhash.Hash) (*wire.MsgBlock, error)
}

// DB abstracts the required persistent calls expected by the lookout. DB
// provides the ability to search for state updates that correspond to breach
// transactions confirmed in a particular block.
type DB interface {
	// GetLookoutTip returns the last block epoch at which the tower
	// performed a match. If no match has been done, a nil epoch will be
	// returned.
	GetLookoutTip() (*chainntnfs.BlockEpoch, error)

	// QueryMatches searches its database for any state updates matching the
	// provided breach hints. If any matches are found, they will be
	// returned along with encrypted blobs so that justice can be exacted.
	QueryMatches([]blob.BreachHint) ([]wtdb.Match, error)

	// SetLookoutTip writes the best epoch for which the watchtower has
	// queried for breach hints.
	SetLookoutTip(*chainntnfs.BlockEpoch) error
}

// EpochRegistrar supports the ability to register for events corresponding to
// newly created blocks.
type EpochRegistrar interface {
	// RegisterBlockEpochNtfn registers for a new block epoch subscription.
	// The implementation must support historical dispatch, starting from
	// the provided chainntnfs.BlockEpoch when it is non-nil. The
	// notifications should be delivered in-order, and deliver reorged
	// blocks.
	RegisterBlockEpochNtfn(
		*chainntnfs.BlockEpoch) (*chainntnfs.BlockEpochEvent, error)
}

// Punisher handles the construction and publication of justice transactions
// once they have been detected by the Service.
type Punisher interface {
	// Punish accepts a JusticeDescriptor, constructs the justice
	// transaction, and publishes the transaction to the network so it can
	// be mined. The second parameter is a quit channel so that long-running
	// operations required to track the confirmation of the transaction can
	// be canceled on shutdown.
	Punish(*JusticeDescriptor, <-chan struct{}) error
}
