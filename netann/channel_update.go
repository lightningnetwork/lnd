package netann

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ChannelUpdateModifier is a closure that makes in-place modifications to an
// lnwire.ChannelUpdate.
type ChannelUpdateModifier func(*lnwire.ChannelUpdate)

// ChannelUpdateSetDisable sets the disabled channel flag if disabled is true,
// and clears the bit otherwise.
func ChannelUpdateSetDisable(disabled bool) ChannelUpdateModifier {
	return func(update *lnwire.ChannelUpdate) {
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

// SignChannelUpdate applies the given modifiers to the passed
// lnwire.ChannelUpdate, then signs the resulting update. The provided update
// should be the most recent, valid update, otherwise the timestamp may not
// monotonically increase from the prior.
//
// NOTE: This method modifies the given update.
func SignChannelUpdate(signer lnwallet.MessageSigner, pubKey *btcec.PublicKey,
	update *lnwire.ChannelUpdate, mods ...ChannelUpdateModifier) error {

	// Apply the requested changes to the channel update.
	for _, modifier := range mods {
		modifier(update)
	}

	// Update the message's timestamp to the current time. If the update's
	// current time is already in the future, we increment the prior value
	// to ensure the timestamps monotonically increase, otherwise the
	// update won't propagate.
	newTimestamp := uint32(time.Now().Unix())
	if newTimestamp <= update.Timestamp {
		newTimestamp = update.Timestamp + 1
	}
	update.Timestamp = newTimestamp

	chanUpdateMsg, err := update.DataToSign()
	if err != nil {
		return err
	}

	// Create the DER-encoded ECDSA signature over the message digest.
	sig, err := signer.SignMessage(pubKey, chanUpdateMsg)
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
	info *channeldb.ChannelEdgeInfo,
	policies ...*channeldb.ChannelEdgePolicy) (
	*lnwire.ChannelUpdate, error) {

	// Helper function to extract the owner of the given policy.
	owner := func(edge *channeldb.ChannelEdgePolicy) []byte {
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

	return nil, fmt.Errorf("unable to extract ChannelUpdate for channel %v",
		info.ChannelPoint)
}

// ChannelUpdateFromEdge reconstructs a signed ChannelUpdate from the given edge
// info and policy.
func ChannelUpdateFromEdge(info *channeldb.ChannelEdgeInfo,
	policy *channeldb.ChannelEdgePolicy) (*lnwire.ChannelUpdate, error) {

	update := &lnwire.ChannelUpdate{
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

	var err error
	update.Signature, err = lnwire.NewSigFromRawSignature(policy.SigBytes)
	if err != nil {
		return nil, err
	}

	return update, nil
}
