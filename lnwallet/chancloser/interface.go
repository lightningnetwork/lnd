package chancloser

import (
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

// CoopFeeEstimator is used to estimate the fee of a co-op close transaction.
type CoopFeeEstimator interface {
	// EstimateFee estimates an _absolute_ fee for a co-op close transaction
	// given the local+remote tx outs (for the co-op close transaction),
	// channel type, and ideal fee rate. If a passed TxOut is nil, then
	// that indicates that an output is dust on the co-op close transaction
	// _before_ fees are accounted for.
	EstimateFee(chanType channeldb.ChannelType,
		localTxOut, remoteTxOut *wire.TxOut,
		idealFeeRate chainfee.SatPerKWeight) btcutil.Amount
}

// Channel abstracts away from the core channel state machine by exposing an
// interface that requires only the methods we need to carry out the channel
// closing process.
type Channel interface { //nolint:interfacebloat
	// ChannelPoint returns the channel point of the target channel.
	ChannelPoint() wire.OutPoint

	// LocalCommitmentBlob may return the auxiliary data storage blob for
	// the local commitment transaction.
	LocalCommitmentBlob() fn.Option[tlv.Blob]

	// FundingBlob may return the auxiliary data storage blob related to
	// funding details for the channel.
	FundingBlob() fn.Option[tlv.Blob]

	// MarkCoopBroadcasted persistently marks that the channel close
	// transaction has been broadcast.
	MarkCoopBroadcasted(*wire.MsgTx, lntypes.ChannelParty) error

	// MarkShutdownSent persists the given ShutdownInfo. The existence of
	// the ShutdownInfo represents the fact that the Shutdown message has
	// been sent by us and so should be re-sent on re-establish.
	MarkShutdownSent(info *channeldb.ShutdownInfo) error

	// IsInitiator returns true we are the initiator of the channel.
	IsInitiator() bool

	// ShortChanID returns the scid of the channel.
	ShortChanID() lnwire.ShortChannelID

	// ChanType returns the channel type of the channel.
	ChanType() channeldb.ChannelType

	// FundingTxOut returns the funding output of the channel.
	FundingTxOut() *wire.TxOut

	// AbsoluteThawHeight returns the absolute thaw height of the channel.
	// If the channel is pending, or an unconfirmed zero conf channel, then
	// an error should be returned.
	AbsoluteThawHeight() (uint32, error)

	// LocalBalanceDust returns true if when creating a co-op close
	// transaction, the balance of the local party will be dust after
	// accounting for any anchor outputs. The dust value for the local
	// party is also returned.
	LocalBalanceDust() (bool, btcutil.Amount)

	// RemoteBalanceDust returns true if when creating a co-op close
	// transaction, the balance of the remote party will be dust after
	// accounting for any anchor outputs. The dust value the remote party
	// is also returned.
	RemoteBalanceDust() (bool, btcutil.Amount)

	// CommitBalances returns the local and remote balances in the current
	// commitment state.
	CommitBalances() (btcutil.Amount, btcutil.Amount)

	// CommitFee returns the commitment fee for the current commitment
	// state.
	CommitFee() btcutil.Amount

	// RemoteUpfrontShutdownScript returns the upfront shutdown script of
	// the remote party. If the remote party didn't specify such a script,
	// an empty delivery address should be returned.
	RemoteUpfrontShutdownScript() lnwire.DeliveryAddress

	// CreateCloseProposal creates a new co-op close proposal in the form
	// of a valid signature, the chainhash of the final txid, and our final
	// balance in the created state.
	CreateCloseProposal(proposedFee btcutil.Amount,
		localDeliveryScript []byte, remoteDeliveryScript []byte,
		closeOpt ...lnwallet.ChanCloseOpt,
	) (
		input.Signature, *wire.MsgTx, btcutil.Amount, error)

	// CompleteCooperativeClose persistently "completes" the cooperative
	// close by producing a fully signed co-op close transaction.
	CompleteCooperativeClose(localSig, remoteSig input.Signature,
		localDeliveryScript, remoteDeliveryScript []byte,
		proposedFee btcutil.Amount, closeOpt ...lnwallet.ChanCloseOpt,
	) (*wire.MsgTx, btcutil.Amount, error)
}

// MusigSession is an interface that abstracts away the details of the musig2
// session details. A session is used to generate the necessary closing options
// needed to close a channel cooperatively.
type MusigSession interface {
	// ProposalClosingOpts generates the set of closing options needed to
	// generate a new musig2 proposal signature.
	ProposalClosingOpts() ([]lnwallet.ChanCloseOpt, error)

	// CombineClosingOpts returns the options that should be used when
	// combining the final musig partial signature. The method also maps
	// the lnwire partial signatures into an input.Signature that can be
	// used more generally.
	CombineClosingOpts(localSig, remoteSig lnwire.PartialSig,
	) (input.Signature, input.Signature, []lnwallet.ChanCloseOpt, error)

	// ClosingNonce generates the nonce we'll use to generate the musig2
	// partial signatures for the co-op close transaction.
	ClosingNonce() (*musig2.Nonces, error)

	// InitRemoteNonce saves the remote nonce the party sent during their
	// shutdown message so it can be used later to generate and verify
	// signatures.
	InitRemoteNonce(nonce *musig2.Nonces)
}
