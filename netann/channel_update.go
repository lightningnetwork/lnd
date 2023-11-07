package netann

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ErrUnableToExtractChanUpdate is returned when a channel update cannot be
// found for one of our active channels.
var ErrUnableToExtractChanUpdate = fmt.Errorf("unable to extract ChannelUpdate")

// ChannelUpdateModifier is a closure that makes in-place modifications to an
// lnwire.ChannelUpdate1.
type ChannelUpdateModifier func(lnwire.ChannelUpdate)

// ChanUpdSetDisable is a functional option that sets the disabled channel flag
// if disabled is true, and clears the bit otherwise.
func ChanUpdSetDisable(disabled bool) ChannelUpdateModifier {
	return func(update lnwire.ChannelUpdate) {
		update.SetDisabled(disabled)
	}
}

// ChanUpdSetTimestamp is a functional option that sets the timestamp of the
// update to the current time, or increments it if the timestamp is already in
// the future.
func ChanUpdSetTimestamp(bestBlockHeight uint32) ChannelUpdateModifier {
	return func(update lnwire.ChannelUpdate) {
		switch upd := update.(type) {
		case *lnwire.ChannelUpdate1:
			newTimestamp := uint32(time.Now().Unix())
			if newTimestamp <= upd.Timestamp {
				// Increment the prior value to ensure the
				// timestamp monotonically increases, otherwise
				// the update won't propagate.
				newTimestamp = upd.Timestamp + 1
			}
			upd.Timestamp = newTimestamp

		case *lnwire.ChannelUpdate2:
			newBlockHeight := bestBlockHeight
			if newBlockHeight <= upd.BlockHeight {
				// Increment the prior value to ensure the
				// blockHeight monotonically increases,
				// otherwise the update won't propagate.
				newBlockHeight = upd.BlockHeight + 1
			}
			upd.BlockHeight = newBlockHeight

		default:
			log.Errorf("unhandled implementation of "+
				"lnwire.ChannelUpdate: %T", update)
		}
	}
}

// SignChannelUpdate applies the given modifiers to the passed
// lnwire.ChannelUpdate1, then signs the resulting update. The provided update
// should be the most recent, valid update, otherwise the timestamp may not
// monotonically increase from the prior.
//
// NOTE: This method modifies the given update.
func SignChannelUpdate(signer keychain.MessageSignerRing,
	keyLoc keychain.KeyLocator, update lnwire.ChannelUpdate,
	mods ...ChannelUpdateModifier) error {

	// Apply the requested changes to the channel update.
	for _, modifier := range mods {
		modifier(update)
	}

	switch upd := update.(type) {
	case *lnwire.ChannelUpdate1:
		data, err := upd.DataToSign()
		if err != nil {
			return err
		}

		sig, err := signer.SignMessage(keyLoc, data, true)
		if err != nil {
			return err
		}

		// Parse the DER-encoded signature into a fixed-size 64-byte
		// array.
		upd.Signature, err = lnwire.NewSigFromSignature(sig)
		if err != nil {
			return err
		}

	case *lnwire.ChannelUpdate2:
		data, err := upd.DataToSign()
		if err != nil {
			return err
		}

		sig, err := signer.SignMessageSchnorr(
			keyLoc, data, false, nil, upd.DigestTag(),
		)
		if err != nil {
			return err
		}

		upd.Signature, err = lnwire.NewSigFromSignature(sig)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unhandled implementaion of "+
			"ChannelUpdate: %T", update)
	}

	return nil
}

// ExtractChannelUpdate attempts to retrieve a lnwire.ChannelUpdate1 message
// from an edge's info and a set of routing policies.
//
// NOTE: The passed policies can be nil.
func ExtractChannelUpdate(ownerPubKey []byte,
	info models.ChannelEdgeInfo, policies ...models.ChannelEdgePolicy) (
	*lnwire.ChannelUpdate1, error) {

	// Helper function to extract the owner of the given policy.
	owner := func(edge models.ChannelEdgePolicy) []byte {
		var pubKey *btcec.PublicKey
		if edge.IsNode1() {
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
			update, err := ChannelUpdateFromEdge(info, edge)
			if err != nil {
				return nil, err
			}

			chanUpd1, ok := update.(*lnwire.ChannelUpdate1)
			if !ok {
				return nil, fmt.Errorf("expected "+
					"*lnwire.ChannelUpdate1, got: %T",
					chanUpd1)
			}

			return chanUpd1, nil
		}
	}

	return nil, ErrUnableToExtractChanUpdate
}

// UnsignedChannelUpdateFromEdge reconstructs an unsigned ChannelUpdate1 from the
// given edge info and policy.
func UnsignedChannelUpdateFromEdge(chainHash chainhash.Hash,
	policy models.ChannelEdgePolicy) (lnwire.ChannelUpdate, error) {

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

	case *models.ChannelEdgePolicy2:
		return &lnwire.ChannelUpdate2{
			ChainHash:                 chainHash,
			ShortChannelID:            p.ShortChannelID,
			BlockHeight:               p.BlockHeight,
			DisabledFlags:             p.DisabledFlags,
			Direction:                 p.Direction,
			CLTVExpiryDelta:           p.CLTVExpiryDelta,
			HTLCMinimumMsat:           p.HTLCMinimumMsat,
			HTLCMaximumMsat:           p.HTLCMaximumMsat,
			FeeBaseMsat:               p.FeeBaseMsat,
			FeeProportionalMillionths: p.FeeProportionalMillionths,
			ExtraOpaqueData:           p.ExtraOpaqueData,
		}, nil

	default:
		return nil, fmt.Errorf("unhandled implementation of the "+
			"models.ChanelEdgePolicy interface: %T", policy)
	}
}

// ChannelUpdateFromEdge reconstructs a signed ChannelUpdate1 from the given
// edge info and policy.
func ChannelUpdateFromEdge(info models.ChannelEdgeInfo,
	policy models.ChannelEdgePolicy) (lnwire.ChannelUpdate, error) {

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
