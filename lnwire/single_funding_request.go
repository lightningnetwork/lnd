package lnwire

import (
	"fmt"
	"io"

	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcutil"
)

// SingleFundingRequest is the message Alice sends to Bob if we should like
// to create a channel with Bob where she's the sole provider of funds to the
// channel. Single funder channels simplify the initial funding workflow, are
// supported by nodes backed by SPV Bitcoin clients, and have a simpler
// security models than dual funded channels.
//
// NOTE: In order to avoid a slow loris like resource exhaustion attack, the
// responder of a single funding channel workflow *should not* watch the
// blockchain for the funding transaction. Instead, it is the initiator's job
// to provide the responder with an SPV proof of funding transaction inclusion
// after a sufficient number of confirmations.
type SingleFundingRequest struct {
	// ChannelID serves to uniquely identify the future channel created by
	// the initiated single funder workflow.
	ChannelID uint64

	// ChannelType represents the type of channel this request would like
	// to open. At this point, the only supported channels are type 0
	// channels, which are channels with regular commitment transactions
	// utilizing HTLC's for payments.
	ChannelType uint8

	// CoinType represents which blockchain the channel will be opened
	// using. By default, this field should be set to 0, indicating usage
	// of the Bitcoin blockchain.
	CoinType uint64

	// FeePerKb is the required number of satoshis per KB that the
	// requester will pay at all timers, for both the funding transaction
	// and commitment transaction. This value can later be updated once the
	// channel is open.
	FeePerKb btcutil.Amount

	// FundingAmount is the number of satoshis the initiator would like
	// to commit to the channel.
	FundingAmount btcutil.Amount

	// PushSatoshis is the number of satoshis that will be pushed to the
	// responder as a part of the first commitment state.
	PushSatoshis btcutil.Amount

	// CsvDelay is the number of blocks to use for the relative time lock
	// in the pay-to-self output of both commitment transactions.
	CsvDelay uint32

	// CommitmentKey is key the initiator of the funding workflow wishes to
	// use within their versino of the commitment transaction for any
	// delayed (CSV) or immediate outputs to them.
	CommitmentKey *btcec.PublicKey

	// ChannelDerivationPoint is an secp256k1 point which will be used to
	// derive the public key the initiator will use for the half of the
	// 2-of-2 multi-sig. Using the channel derivation point (CDP), and the
	// initiators identity public key (A), the channel public key is
	// computed as: C = A + CDP. In order to be valid all CDP's MUST have
	// an odd y-coordinate.
	ChannelDerivationPoint *btcec.PublicKey

	// DeliveryPkScript defines the public key script that the initiator
	// would like to use to receive their balance in the case of a
	// cooperative close. Only the following script templates are
	// supported: P2PKH, P2WKH, P2SH, and P2WSH.
	DeliveryPkScript PkScript

	// DustLimit is the threshold below which no HTLC output should be
	// generated for our commitment transaction; ie. HTLCs below
	// this amount are not enforceable onchain from our point view.
	DustLimit btcutil.Amount

	// TODO(roasbeef): confirmation depth
}

// NewSingleFundingRequest creates, and returns a new empty SingleFundingRequest.
func NewSingleFundingRequest(chanID uint64, chanType uint8, coinType uint64,
	fee btcutil.Amount, amt btcutil.Amount, delay uint32, ck,
	cdp *btcec.PublicKey, deliveryScript PkScript,
	dustLimit btcutil.Amount, pushSat btcutil.Amount) *SingleFundingRequest {

	return &SingleFundingRequest{
		ChannelID:              chanID,
		ChannelType:            chanType,
		CoinType:               coinType,
		FeePerKb:               fee,
		FundingAmount:          amt,
		CsvDelay:               delay,
		CommitmentKey:          ck,
		ChannelDerivationPoint: cdp,
		DeliveryPkScript:       deliveryScript,
		DustLimit:              dustLimit,
		PushSatoshis:           pushSat,
	}
}

// Decode deserializes the serialized SingleFundingRequest stored in the passed
// io.Reader into the target SingleFundingRequest using the deserialization
// rules defined by the passed protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) Decode(r io.Reader, pver uint32) error {
	// ChannelID (8)
	// ChannelType (1)
	// CoinType	(8)
	// FeePerKb (8)
	// PaymentAmount (8)
	// Delay (4)
	// Pubkey (33)
	// Pubkey (33)
	// DeliveryPkScript (final delivery)
	// DustLimit (8)
	err := readElements(r,
		&c.ChannelID,
		&c.ChannelType,
		&c.CoinType,
		&c.FeePerKb,
		&c.FundingAmount,
		&c.PushSatoshis,
		&c.CsvDelay,
		&c.CommitmentKey,
		&c.ChannelDerivationPoint,
		&c.DeliveryPkScript,
		&c.DustLimit)
	if err != nil {
		return err
	}

	return nil
}

// Encode serializes the target SingleFundingRequest into the passed io.Writer
// implementation. Serialization will observe the rules defined by the passed
// protocol version.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) Encode(w io.Writer, pver uint32) error {
	// ChannelID (8)
	// ChannelType (1)
	// CoinType	(8)
	// FeePerKb (8)
	// PaymentAmount (8)
	// Delay (4)
	// Pubkey (33)
	// Pubkey (33)
	// DeliveryPkScript (final delivery)
	// DustLimit (8)
	err := writeElements(w,
		c.ChannelID,
		c.ChannelType,
		c.CoinType,
		c.FeePerKb,
		c.FundingAmount,
		c.PushSatoshis,
		c.CsvDelay,
		c.CommitmentKey,
		c.ChannelDerivationPoint,
		c.DeliveryPkScript,
		c.DustLimit)
	if err != nil {
		return err
	}

	return nil
}

// Command returns the uint32 code which uniquely identifies this message as a
// SingleFundingRequest on the wire.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) Command() uint32 {
	return CmdSingleFundingRequest
}

// MaxPayloadLength returns the maximum allowed payload length for a
// SingleFundingRequest. This is calculated by summing the max length of all
// the fields within a SingleFundingRequest. To enforce a maximum
// DeliveryPkScript size, the size of a P2PKH public key script is used.
// Therefore, the final breakdown is: 8 + 1 + 8 + 8 + 8 + 4 + 33 + 33 + 25 + 8
// + 9 = 166.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) MaxPayloadLength(uint32) uint32 {
	return 174
}

// Validate examines each populated field within the SingleFundingRequest for
// field sanity. For example, all fields MUST NOT be negative, and all pkScripts
// must belong to the allowed set of public key scripts.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) Validate() error {
	// Negative values is are allowed.
	if c.FeePerKb < 0 {
		return fmt.Errorf("MinFeePerKb cannot be negative")
	}
	if c.FundingAmount < 0 {
		return fmt.Errorf("FundingAmount cannot be negative")
	}

	// The CSV delay MUST be non-zero.
	if c.CsvDelay == 0 {
		return fmt.Errorf("Commitment transaction must have non-zero " +
			"CSV delay")
	}

	// The channel derivation point must be non-nil, and have an odd
	// y-coordinate.
	if c.ChannelDerivationPoint == nil {
		return fmt.Errorf("The channel derivation point must be non-nil")
	}
	//if c.ChannelDerivationPoint.Y.Bit(0) != 1 {
	//return fmt.Errorf("The channel derivation point must have an odd " +
	//"y-coordinate")
	//}

	// The delivery pkScript must be amongst the supported script
	// templates.
	if !isValidPkScript(c.DeliveryPkScript) {
		// TODO(roasbeef): move into actual error
		return fmt.Errorf("Valid delivery public key scripts MUST be: " +
			"P2PKH, P2WKH, P2SH, or P2WSH.")
	}

	if c.DustLimit <= 0 {
		return fmt.Errorf("Dust limit should be greater than zero.")
	}

	// We're good!
	return nil
}

// String returns the string representation of the SingleFundingRequest.
//
// This is part of the lnwire.Message interface.
func (c *SingleFundingRequest) String() string {
	var serializedPubkey []byte
	if &c.ChannelDerivationPoint != nil && c.ChannelDerivationPoint.X != nil {
		serializedPubkey = c.ChannelDerivationPoint.SerializeCompressed()
	}

	// TODO(roasbeef): remove string methods?

	return fmt.Sprintf("\n--- Begin SingleFundingRequest ---\n") +
		fmt.Sprintf("ChannelID:\t\t\t%d\n", c.ChannelID) +
		fmt.Sprintf("ChannelType:\t\t\t%x\n", c.ChannelType) +
		fmt.Sprintf("CoinType:\t\t\t%d\n", c.CoinType) +
		fmt.Sprintf("FeePerKb:\t\t\t%s\n", c.FeePerKb.String()) +
		fmt.Sprintf("FundingAmount:\t\t\t%s\n", c.FundingAmount.String()) +
		fmt.Sprintf("PushSatoshis:\t\t%v\n", c.PushSatoshis) +
		fmt.Sprintf("CsvDelay:\t\t\t%d\n", c.CsvDelay) +
		fmt.Sprintf("ChannelDerivationPoint:\t\t\t%x\n", serializedPubkey) +
		fmt.Sprintf("DeliveryPkScript:\t\t\t%x\n", c.DeliveryPkScript) +
		fmt.Sprintf("DustLimit:\t\t\t%d\n", c.DustLimit) +
		fmt.Sprintf("--- End SingleFundingRequest ---\n")
}
