package chancloser

import (
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/types"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/routing/route"
)

// AuxCloseOutputs is used to specify extra outputs that should be used when
// constructing the co-op close transaction.
type AuxCloseOutputs struct {
	// ExtraCloseOutputs is a set of extra outputs that should be included
	// in the close transaction.
	ExtraCloseOutputs []lnwallet.CloseOutput

	// CustomSort is a custom function that can be used to sort the
	// transaction outputs. If this isn't set, then the default BIP-69
	// sorting is used.
	CustomSort lnwallet.CloseSortFunc
}

// AuxCloseOutputShape describes a single auxiliary close output in a
// fee-independent way, capturing only the properties that contribute to the
// weight of the co-op close transaction.
type AuxCloseOutputShape struct {
	// IsLocal is true if the output belongs to the local party.
	IsLocal bool

	// PkScriptSize is the size, in bytes, of the output's pkScript.
	PkScriptSize int
}

// AuxCloseShape is the fee-independent shape of the auxiliary outputs that
// will be added to the co-op close transaction: their number and pkScript
// sizes. The values of the concrete outputs may still depend on the
// negotiated close fee, but values don't contribute to transaction weight.
type AuxCloseShape struct {
	// Outputs describes each auxiliary close output.
	Outputs []AuxCloseOutputShape
}

// AuxChanCloser is used to allow an external caller to modify the co-op close
// transaction.
type AuxChanCloser interface {
	// ShutdownBlob returns the set of custom records that should be
	// included in the shutdown message.
	ShutdownBlob(req types.AuxShutdownReq) (fn.Option[lnwire.CustomRecords],
		error)

	// AuxCloseShape returns the fee-independent shape of the auxiliary
	// outputs required to close the channel. The shape determines the
	// transaction weight used for fee negotiation, so it MUST NOT depend
	// on the close fee, while the values of the concrete outputs returned
	// by AuxCloseOutputs may.
	AuxCloseShape(desc types.AuxCloseShapeDesc) (fn.Option[AuxCloseShape],
		error)

	// AuxCloseOutputs returns the set of custom outputs that should be used
	// to construct the co-op close transaction. The legacy close
	// negotiation asks for them anew for every fee that is proposed.
	AuxCloseOutputs(desc types.AuxCloseDesc) (fn.Option[AuxCloseOutputs],
		error)

	// AuxCloseTemplate returns the custom outputs every close transaction
	// of an RBF close negotiation carries, along with the sort of those
	// transactions. They're requested once per negotiation, without a
	// fee, so neither the outputs nor their order may depend on the close
	// fee or the amounts of the settled outputs. The aux closer keeps the
	// template for the whole close of the channel: a later call with the
	// same description, after a reconnect or a restart, returns the same
	// template, and a call with a different description fails.
	AuxCloseTemplate(
		desc types.AuxCloseTemplateDesc) (fn.Option[AuxCloseOutputs],
		error)

	// FinalizeClose is called after the close transaction has been agreed
	// upon.
	FinalizeClose(desc types.AuxCloseDesc, closeTx *wire.MsgTx) error

	// SupportsRbfClose returns true if the channel identified by the
	// given channel ID can be closed with the RBF co-op close flow with
	// the given peer. The RBF feature bits are negotiated at the peer
	// level, so this is how an aux channel opts into the flow on a
	// per-channel basis: both peers need an aux closer that drives the
	// aux hooks from within the RBF state machine.
	SupportsRbfClose(chanID lnwire.ChannelID, peer route.Vertex) bool
}
