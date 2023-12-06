package netann

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrUnableToExtractChanUpdate is returned when a channel update cannot be
// found for one of our active channels.
var ErrUnableToExtractChanUpdate = fmt.Errorf("unable to extract ChannelUpdate")

// ChannelUpdateModifier is a closure that makes in-place modifications to an
// lnwire.ChannelUpdate1.
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
// lnwire.ChannelUpdate1, then signs the resulting update. The provided update
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

// ExtractChannelUpdate attempts to retrieve a lnwire.ChannelUpdate1 message
// from an edge's info and a set of routing policies.
//
// NOTE: The passed policies can be nil.
func ExtractChannelUpdate(ownerPubKey []byte,
	info models.ChannelEdgeInfo, policies ...*models.ChannelEdgePolicy1) (
	*lnwire.ChannelUpdate1, error) {

	// Helper function to extract the owner of the given policy.
	owner := func(edge *models.ChannelEdgePolicy1) []byte {
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

// UnsignedChannelUpdateFromEdge reconstructs an unsigned ChannelUpdate1 from the
// given edge info and policy.
func UnsignedChannelUpdateFromEdge(chainHash chainhash.Hash,
	policy models.ChannelEdgePolicy) (*lnwire.ChannelUpdate1, error) {

	switch p := policy.(type) {
	case *models.ChannelEdgePolicy1:
		return &lnwire.ChannelUpdate1{
			ChainHash: chainHash,
			ShortChannelID: lnwire.NewShortChanIDFromInt(
				p.ChannelID,
			),
			Timestamp:       uint32(p.LastUpdate.Unix()),
			ChannelFlags:    p.ChannelFlags,
			MessageFlags:    p.MessageFlags,
			TimeLockDelta:   p.TimeLockDelta,
			HtlcMinimumMsat: p.MinHTLC,
			HtlcMaximumMsat: p.MaxHTLC,
			BaseFee:         uint32(p.FeeBaseMSat),
			FeeRate:         uint32(p.FeeProportionalMillionths),
			ExtraOpaqueData: p.ExtraOpaqueData,
		}, nil

	default:
		return nil, fmt.Errorf("unhandled implementation of the "+
			"models.ChanelEdgePolicy interface: %T", policy)
	}
}

// ChannelUpdateFromEdge reconstructs a signed ChannelUpdate1 from the given
// edge info and policy.
func ChannelUpdateFromEdge(info models.ChannelEdgeInfo,
	policy models.ChannelEdgePolicy) (*lnwire.ChannelUpdate1, error) {

	update, err := UnsignedChannelUpdateFromEdge(
		info.GetChainHash(), policy,
	)
	if err != nil {
		return nil, err
	}

	sig, err := policy.Sig()
	if err != nil {
		return nil, err
	}

	err = update.SetSig(sig)
	if err != nil {
		return nil, err
	}

	return update, nil
}
