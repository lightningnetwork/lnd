package chanstate

import (
	"io"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelStateBounds are the parameters from OpenChannel and AcceptChannel
// that are responsible for providing bounds on the state space of the abstract
// channel state. These values must be remembered for normal channel operation
// but they do not impact how we compute the commitment transactions themselves.
type ChannelStateBounds struct {
	// ChanReserve is an absolute reservation on the channel for the
	// owner of this set of constraints. This means that the current
	// settled balance for this node CANNOT dip below the reservation
	// amount. This acts as a defense against costless attacks when
	// either side no longer has any skin in the game.
	ChanReserve btcutil.Amount

	// MaxPendingAmount is the maximum pending HTLC value that the
	// owner of these constraints can offer the remote node at a
	// particular time.
	MaxPendingAmount lnwire.MilliSatoshi

	// MinHTLC is the minimum HTLC value that the owner of these
	// constraints can offer the remote node. If any HTLCs below this
	// amount are offered, then the HTLC will be rejected. This, in
	// tandem with the dust limit allows a node to regulate the
	// smallest HTLC that it deems economically relevant.
	MinHTLC lnwire.MilliSatoshi

	// MaxAcceptedHtlcs is the maximum number of HTLCs that the owner of
	// this set of constraints can offer the remote node. This allows each
	// node to limit their over all exposure to HTLCs that may need to be
	// acted upon in the case of a unilateral channel closure or a contract
	// breach.
	MaxAcceptedHtlcs uint16
}

// CommitmentParams are the parameters from OpenChannel and
// AcceptChannel that are required to render an abstract channel state to a
// concrete commitment transaction. These values are necessary to (re)compute
// the commitment transaction. We treat these differently than the state space
// bounds because their history needs to be stored in order to properly handle
// chain resolution.
type CommitmentParams struct {
	// DustLimit is the threshold (in satoshis) below which any outputs
	// should be trimmed. When an output is trimmed, it isn't materialized
	// as an actual output, but is instead burned to miner's fees.
	DustLimit btcutil.Amount

	// CsvDelay is the relative time lock delay expressed in blocks. Any
	// settled outputs that pay to the owner of this channel configuration
	// MUST ensure that the delay branch uses this value as the relative
	// time lock. Similarly, any HTLC's offered by this node should use
	// this value as well.
	CsvDelay uint16
}

// ChannelConfig is a struct that houses the various configuration opens for
// channels. Each side maintains an instance of this configuration file as it
// governs: how the funding and commitment transaction to be created, the
// nature of HTLC's allotted, the keys to be used for delivery, and relative
// time lock parameters.
type ChannelConfig struct {
	// ChannelStateBounds is the set of constraints that must be
	// upheld for the duration of the channel for the owner of this channel
	// configuration. Constraints govern a number of flow control related
	// parameters, also including the smallest HTLC that will be accepted
	// by a participant.
	ChannelStateBounds

	// CommitmentParams is an embedding of the parameters
	// required to render an abstract channel state into a concrete
	// commitment transaction.
	CommitmentParams

	// MultiSigKey is the key to be used within the 2-of-2 output script
	// for the owner of this channel config.
	MultiSigKey keychain.KeyDescriptor

	// RevocationBasePoint is the base public key to be used when deriving
	// revocation keys for the remote node's commitment transaction. This
	// will be combined along with a per commitment secret to derive a
	// unique revocation key for each state.
	RevocationBasePoint keychain.KeyDescriptor

	// PaymentBasePoint is the base public key to be used when deriving
	// the key used within the non-delayed pay-to-self output on the
	// commitment transaction for a node. This will be combined with a
	// tweak derived from the per-commitment point to ensure unique keys
	// for each commitment transaction.
	PaymentBasePoint keychain.KeyDescriptor

	// DelayBasePoint is the base public key to be used when deriving the
	// key used within the delayed pay-to-self output on the commitment
	// transaction for a node. This will be combined with a tweak derived
	// from the per-commitment point to ensure unique keys for each
	// commitment transaction.
	DelayBasePoint keychain.KeyDescriptor

	// HtlcBasePoint is the base public key to be used when deriving the
	// local HTLC key. The derived key (combined with the tweak derived
	// from the per-commitment point) is used within the "to self" clause
	// within any HTLC output scripts.
	HtlcBasePoint keychain.KeyDescriptor
}

// WriteChanConfig serializes a channel config.
func WriteChanConfig(b io.Writer, c *ChannelConfig) error {
	return WriteElements(b,
		c.DustLimit, c.MaxPendingAmount, c.ChanReserve, c.MinHTLC,
		c.MaxAcceptedHtlcs, c.CsvDelay, c.MultiSigKey,
		c.RevocationBasePoint, c.PaymentBasePoint, c.DelayBasePoint,
		c.HtlcBasePoint,
	)
}

// ReadChanConfig deserializes a channel config.
func ReadChanConfig(b io.Reader, c *ChannelConfig) error {
	return ReadElements(b,
		&c.DustLimit, &c.MaxPendingAmount, &c.ChanReserve,
		&c.MinHTLC, &c.MaxAcceptedHtlcs, &c.CsvDelay,
		&c.MultiSigKey, &c.RevocationBasePoint,
		&c.PaymentBasePoint, &c.DelayBasePoint,
		&c.HtlcBasePoint,
	)
}
