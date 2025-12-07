package netann

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnutils"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// chanUpdate2MsgName is a string representing the name of the
	// ChannelUpdate2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the ChannelUpdate2 message.
	chanUpdate2MsgName = "channel_update_2"

	// chanUpdate2SigField is the name of the signature field of the
	// ChannelUpdate2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the ChannelUpdate2 message.
	chanUpdate2SigField = "signature"
)

// ErrUnableToExtractChanUpdate is returned when a channel update cannot be
// found for one of our active channels.
var ErrUnableToExtractChanUpdate = fmt.Errorf("unable to extract ChannelUpdate")

// ChannelUpdateModifier is a closure that makes in-place modifications to an
// lnwire.ChannelUpdate.
type ChannelUpdateModifier func(*lnwire.ChannelUpdate1)

// ChanUpdSetDisable is a functional option that sets the disabled channel flag
// if disabled is true, and clears the bit otherwise.
func ChanUpdSetDisable(disabled bool) ChannelUpdateModifier {
	return func(update *lnwire.ChannelUpdate1) {
		if disabled {
			// Set the bit responsible for marking a channel as
			// disabled.
			update.ChannelFlags |= lnwire.ChanUpdateDisabled
		} else {
			// Clear the bit responsible for marking a channel as
			// disabled.
			update.ChannelFlags &= ^lnwire.ChanUpdateDisabled
		}
	}
}

// ChanUpdSetTimestamp is a functional option that sets the timestamp of the
// update to the current time, or increments it if the timestamp is already in
// the future.
func ChanUpdSetTimestamp(update *lnwire.ChannelUpdate1) {
	newTimestamp := uint32(time.Now().Unix())
	if newTimestamp <= update.Timestamp {
		// Increment the prior value to ensure the timestamp
		// monotonically increases, otherwise the update won't
		// propagate.
		newTimestamp = update.Timestamp + 1
	}
	update.Timestamp = newTimestamp
}

// SignChannelUpdate applies the given modifiers to the passed
// lnwire.ChannelUpdate, then signs the resulting update. The provided update
// should be the most recent, valid update, otherwise the timestamp may not
// monotonically increase from the prior.
//
// NOTE: This method modifies the given update.
func SignChannelUpdate(signer lnwallet.MessageSigner, keyLoc keychain.KeyLocator,
	update *lnwire.ChannelUpdate1, mods ...ChannelUpdateModifier) error {

	// Apply the requested changes to the channel update.
	for _, modifier := range mods {
		modifier(update)
	}

	// Create the DER-encoded ECDSA signature over the message digest.
	sig, err := SignAnnouncement(signer, keyLoc, update)
	if err != nil {
		return err
	}

	// Parse the DER-encoded signature into a fixed-size 64-byte array.
	update.Signature, err = lnwire.NewSigFromSignature(sig)
	if err != nil {
		return err
	}

	return nil
}

// ExtractChannelUpdate attempts to retrieve a lnwire.ChannelUpdate message from
// an edge's info and a set of routing policies.
//
// NOTE: The passed policies can be nil.
func ExtractChannelUpdate(ownerPubKey []byte,
	info *models.ChannelEdgeInfo,
	policies ...*models.ChannelEdgePolicy) (
	*lnwire.ChannelUpdate1, error) {

	// Helper function to extract the owner of the given policy.
	owner := func(edge *models.ChannelEdgePolicy) []byte {
		var pubKey *btcec.PublicKey
		if edge.ChannelFlags&lnwire.ChanUpdateDirection == 0 {
			pubKey, _ = info.NodeKey1()
		} else {
			pubKey, _ = info.NodeKey2()
		}

		// If pubKey was not found, just return nil.
		if pubKey == nil {
			return nil
		}

		return pubKey.SerializeCompressed()
	}

	// Extract the channel update from the policy we own, if any.
	for _, edge := range policies {
		if edge != nil && bytes.Equal(ownerPubKey, owner(edge)) {
			return ChannelUpdateFromEdge(info, edge)
		}
	}

	return nil, ErrUnableToExtractChanUpdate
}

// UnsignedChannelUpdateFromEdge reconstructs an unsigned ChannelUpdate from the
// given edge info and policy.
func UnsignedChannelUpdateFromEdge(info *models.ChannelEdgeInfo,
	policy *models.ChannelEdgePolicy) *lnwire.ChannelUpdate1 {

	update := &lnwire.ChannelUpdate1{
		ChainHash:       info.ChainHash,
		ShortChannelID:  lnwire.NewShortChanIDFromInt(policy.ChannelID),
		Timestamp:       uint32(policy.LastUpdate.Unix()),
		ChannelFlags:    policy.ChannelFlags,
		MessageFlags:    policy.MessageFlags,
		TimeLockDelta:   policy.TimeLockDelta,
		HtlcMinimumMsat: policy.MinHTLC,
		HtlcMaximumMsat: policy.MaxHTLC,
		BaseFee:         uint32(policy.FeeBaseMSat),
		FeeRate:         uint32(policy.FeeProportionalMillionths),
		ExtraOpaqueData: policy.ExtraOpaqueData,
	}
	policy.InboundFee.WhenSome(func(fee lnwire.Fee) {
		update.InboundFee = tlv.SomeRecordT(
			tlv.NewRecordT[tlv.TlvType55555, lnwire.Fee](fee),
		)
	})

	return update
}

// ChannelUpdateFromEdge reconstructs a signed ChannelUpdate from the given edge
// info and policy.
func ChannelUpdateFromEdge(info *models.ChannelEdgeInfo,
	policy *models.ChannelEdgePolicy) (*lnwire.ChannelUpdate1, error) {

	update := UnsignedChannelUpdateFromEdge(info, policy)

	var err error
	update.Signature, err = lnwire.NewSigFromECDSARawSignature(
		policy.SigBytes,
	)
	if err != nil {
		return nil, err
	}

	return update, nil
}

// ValidateChannelUpdateAnn validates the channel update announcement by
// checking (1) that the included signature covers the announcement and has been
// signed by the node's private key, and (2) that the announcement's message
// flags and optional fields are sane.
func ValidateChannelUpdateAnn(pubKey *btcec.PublicKey, capacity btcutil.Amount,
	a lnwire.ChannelUpdate) error {

	if err := ValidateChannelUpdateFields(capacity, a); err != nil {
		return err
	}

	return VerifyChannelUpdateSignature(a, pubKey)
}

// VerifyChannelUpdateSignature verifies that the channel update message was
// signed by the party with the given node public key.
func VerifyChannelUpdateSignature(msg lnwire.ChannelUpdate,
	pubKey *btcec.PublicKey) error {

	switch u := msg.(type) {
	case *lnwire.ChannelUpdate1:
		return verifyChannelUpdate1Signature(u, pubKey)
	case *lnwire.ChannelUpdate2:
		return verifyChannelUpdate2Signature(u, pubKey)
	default:
		return fmt.Errorf("unhandled implementation of "+
			"lnwire.ChannelUpdate: %T", msg)
	}
}

// verifyChannelUpdateSignature1 verifies that the channel update message was
// signed by the party with the given node public key.
func verifyChannelUpdate1Signature(msg *lnwire.ChannelUpdate1,
	pubKey *btcec.PublicKey) error {

	data, err := msg.DataToSign()
	if err != nil {
		return fmt.Errorf("unable to reconstruct message data: %w", err)
	}
	dataHash := chainhash.DoubleHashB(data)

	nodeSig, err := msg.Signature.ToSignature()
	if err != nil {
		return err
	}

	if !nodeSig.Verify(dataHash, pubKey) {
		return fmt.Errorf("invalid signature for channel update %v",
			lnutils.SpewLogClosure(msg))
	}

	return nil
}

// verifyChannelUpdateSignature2 verifies that the channel update message was
// signed by the party with the given node public key.
func verifyChannelUpdate2Signature(c *lnwire.ChannelUpdate2,
	pubKey *btcec.PublicKey) error {

	digest, err := chanUpdate2DigestToSign(c)
	if err != nil {
		return fmt.Errorf("unable to reconstruct message data: %w", err)
	}

	nodeSig, err := c.Signature.Val.ToSignature()
	if err != nil {
		return err
	}

	if !nodeSig.Verify(digest, pubKey) {
		return fmt.Errorf("invalid signature for channel update %v",
			lnutils.SpewLogClosure(c))
	}

	return nil
}

// ValidateChannelUpdateFields validates a channel update's message flags and
// corresponding update fields.
func ValidateChannelUpdateFields(capacity btcutil.Amount,
	msg lnwire.ChannelUpdate) error {

	switch u := msg.(type) {
	case *lnwire.ChannelUpdate1:
		return validateChannelUpdate1Fields(capacity, u)
	case *lnwire.ChannelUpdate2:
		return validateChannelUpdate2Fields(capacity, u)
	default:
		return fmt.Errorf("unhandled implementation of "+
			"lnwire.ChannelUpdate: %T", msg)
	}
}

// validateChannelUpdate1Fields validates a channel update's message flags and
// corresponding update fields.
func validateChannelUpdate1Fields(capacity btcutil.Amount,
	msg *lnwire.ChannelUpdate1) error {

	// The maxHTLC flag is mandatory.
	if !msg.MessageFlags.HasMaxHtlc() {
		return fmt.Errorf("max htlc flag not set for channel "+
			"update %v", lnutils.SpewLogClosure(msg))
	}

	maxHtlc := msg.HtlcMaximumMsat
	if maxHtlc == 0 || maxHtlc < msg.HtlcMinimumMsat {
		return fmt.Errorf("invalid max htlc for channel "+
			"update %v", lnutils.SpewLogClosure(msg))
	}

	// For light clients, the capacity will not be set so we'll skip
	// checking whether the MaxHTLC value respects the channel's
	// capacity.
	capacityMsat := lnwire.NewMSatFromSatoshis(capacity)
	if capacityMsat != 0 && maxHtlc > capacityMsat {
		return fmt.Errorf("max_htlc (%v) for channel update "+
			"greater than capacity (%v)", maxHtlc, capacityMsat)
	}

	return nil
}

// validateChannelUpdate2Fields validates a channel update's message flags and
// corresponding update fields.
func validateChannelUpdate2Fields(capacity btcutil.Amount,
	c *lnwire.ChannelUpdate2) error {

	maxHtlc := c.HTLCMaximumMsat.Val
	if maxHtlc == 0 || maxHtlc < c.HTLCMinimumMsat.Val {
		return fmt.Errorf("invalid max htlc for channel update %v",
			lnutils.SpewLogClosure(c))
	}

	// Checking whether the MaxHTLC value respects the channel's capacity.
	capacityMsat := lnwire.NewMSatFromSatoshis(capacity)
	if maxHtlc > capacityMsat {
		return fmt.Errorf("max_htlc (%v) for channel update greater "+
			"than capacity (%v)", maxHtlc, capacityMsat)
	}

	return nil
}

// ChanUpdate2DigestTag returns the tag to be used when signing the digest of
// a channel_update_2 message.
func ChanUpdate2DigestTag() []byte {
	return MsgTag(chanUpdate2MsgName, chanUpdate2SigField)
}

// chanUpdate2DigestToSign computes the digest of the ChannelUpdate2 message to
// be signed.
func chanUpdate2DigestToSign(c *lnwire.ChannelUpdate2) ([]byte, error) {
	data, err := lnwire.SerialiseFieldsToSign(c)
	if err != nil {
		return nil, err
	}

	hash := MsgHash(chanUpdate2MsgName, chanUpdate2SigField, data)

	return hash[:], nil
}
