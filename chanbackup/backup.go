package chanbackup

import (
	"context"
	"fmt"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
)

// LiveChannelSource is an interface that allows us to query for the set of
// live channels. A live channel is one that is open, and has not had a
// commitment transaction broadcast.
type LiveChannelSource interface {
	// FetchAllChannels returns all known live channels.
	FetchAllChannels() ([]*channeldb.OpenChannel, error)

	// FetchChannel attempts to locate a live channel identified by the
	// passed chanPoint. Optionally an existing db tx can be supplied.
	FetchChannel(chanPoint wire.OutPoint) (*channeldb.OpenChannel, error)
}

// assembleChanBackup attempts to assemble a static channel backup for the
// passed open channel. The backup includes all information required to restore
// the channel, as well as addressing information so we can find the peer and
// reconnect to them to initiate the protocol.
func assembleChanBackup(ctx context.Context, addrSource channeldb.AddrSource,
	openChan *channeldb.OpenChannel) (*Single, error) {

	log.Debugf("Crafting backup for ChannelPoint(%v)",
		openChan.FundingOutpoint)

	// First, we'll query the channel source to obtain all the addresses
	// that are associated with the peer for this channel.
	known, nodeAddrs, err := addrSource.AddrsForNode(
		ctx, openChan.IdentityPub,
	)
	if err != nil {
		return nil, err
	}
	if !known {
		return nil, fmt.Errorf("node unknown by address source")
	}

	single := NewSingle(openChan, nodeAddrs)

	return &single, nil
}

// buildCloseTxInputs generates inputs needed to force close a channel from
// an open channel. Anyone having these inputs and the signer, can sign the
// force closure transaction. Warning! If the channel state updates, an attempt
// to close the channel using this method with outdated CloseTxInputs can result
// in loss of funds! This may happen if an outdated channel backup is attempted
// to be used to force close the channel.
func buildCloseTxInputs(
	targetChan *channeldb.OpenChannel) fn.Option[CloseTxInputs] {

	log.Debugf("Crafting CloseTxInputs for ChannelPoint(%v)",
		targetChan.FundingOutpoint)

	localCommit := targetChan.LocalCommitment

	if localCommit.CommitTx == nil {
		log.Infof("CommitTx is nil for ChannelPoint(%v), "+
			"skipping CloseTxInputs. This is possible when "+
			"DLP is active.", targetChan.FundingOutpoint)

		return fn.None[CloseTxInputs]()
	}

	// We need unsigned force close tx and the counterparty's signature.
	inputs := CloseTxInputs{
		CommitTx:  localCommit.CommitTx,
		CommitSig: localCommit.CommitSig,
	}

	// In case of a taproot channel, commit height is needed as well to
	// produce verification nonce for the taproot channel using shachain.
	if targetChan.ChanType.IsTaproot() {
		inputs.CommitHeight = localCommit.CommitHeight
	}

	// In case of a custom taproot channel, TapscriptRoot is needed as well.
	if targetChan.ChanType.HasTapscriptRoot() {
		inputs.TapscriptRoot = targetChan.TapscriptRoot
	}

	return fn.Some(inputs)
}

// FetchBackupForChan attempts to create a plaintext static channel backup for
// the target channel identified by its channel point. If we're unable to find
// the target channel, then an error will be returned.
func FetchBackupForChan(ctx context.Context, chanPoint wire.OutPoint,
	chanSource LiveChannelSource,
	addrSource channeldb.AddrSource) (*Single, error) {

	// First, we'll query the channel source to see if the channel is known
	// and open within the database.
	targetChan, err := chanSource.FetchChannel(chanPoint)
	if err != nil {
		// If we can't find the channel, then we return with an error,
		// as we have nothing to  backup.
		return nil, fmt.Errorf("unable to find target channel")
	}

	// Once we have the target channel, we can assemble the backup using
	// the source to obtain any extra information that we may need.
	staticChanBackup, err := assembleChanBackup(ctx, addrSource, targetChan)
	if err != nil {
		return nil, fmt.Errorf("unable to create chan backup: %w", err)
	}

	return staticChanBackup, nil
}

// FetchStaticChanBackups will return a plaintext static channel back up for
// all known active/open channels within the passed channel source.
func FetchStaticChanBackups(ctx context.Context, chanSource LiveChannelSource,
	addrSource channeldb.AddrSource) ([]Single, error) {

	// First, we'll query the backup source for information concerning all
	// currently open and available channels.
	openChans, err := chanSource.FetchAllChannels()
	if err != nil {
		return nil, err
	}

	// Now that we have all the channels, we'll use the chanSource to
	// obtain any auxiliary information we need to craft a backup for each
	// channel.
	staticChanBackups := make([]Single, 0, len(openChans))
	for _, openChan := range openChans {
		chanBackup, err := assembleChanBackup(ctx, addrSource, openChan)
		if err != nil {
			return nil, err
		}

		staticChanBackups = append(staticChanBackups, *chanBackup)
	}

	return staticChanBackups, nil
}
