package chanbackup

import (
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/kvdb"
)

// LiveChannelSource is an interface that allows us to query for the set of
// live channels. A live channel is one that is open, and has not had a
// commitment transaction broadcast.
type LiveChannelSource interface {
	// FetchAllChannels returns all known live channels.
	FetchAllChannels() ([]*channeldb.OpenChannel, error)

	// FetchChannel attempts to locate a live channel identified by the
	// passed chanPoint. Optionally an existing db tx can be supplied.
	FetchChannel(tx kvdb.RTx, chanPoint wire.OutPoint) (
		*channeldb.OpenChannel, error)
}

// AddressSource is an interface that allows us to query for the set of
// addresses a node can be connected to.
type AddressSource interface {
	// AddrsForNode returns all known addresses for the target node public
	// key.
	AddrsForNode(nodePub *btcec.PublicKey) ([]net.Addr, error)
}

// assembleChanBackup attempts to assemble a static channel backup for the
// passed open channel. The backup includes all information required to restore
// the channel, as well as addressing information so we can find the peer and
// reconnect to them to initiate the protocol.
func assembleChanBackup(addrSource AddressSource,
	openChan *channeldb.OpenChannel) (*Single, error) {

	log.Debugf("Crafting backup for ChannelPoint(%v)",
		openChan.FundingOutpoint)

	// First, we'll query the channel source to obtain all the addresses
	// that are associated with the peer for this channel.
	nodeAddrs, err := addrSource.AddrsForNode(openChan.IdentityPub)
	if err != nil {
		return nil, err
	}

	single := NewSingle(openChan, nodeAddrs)

	return &single, nil
}

// FetchBackupForChan attempts to create a plaintext static channel backup for
// the target channel identified by its channel point. If we're unable to find
// the target channel, then an error will be returned.
func FetchBackupForChan(chanPoint wire.OutPoint, chanSource LiveChannelSource,
	addrSource AddressSource) (*Single, error) {

	// First, we'll query the channel source to see if the channel is known
	// and open within the database.
	targetChan, err := chanSource.FetchChannel(nil, chanPoint)
	if err != nil {
		// If we can't find the channel, then we return with an error,
		// as we have nothing to  backup.
		return nil, fmt.Errorf("unable to find target channel")
	}

	// Once we have the target channel, we can assemble the backup using
	// the source to obtain any extra information that we may need.
	staticChanBackup, err := assembleChanBackup(addrSource, targetChan)
	if err != nil {
		return nil, fmt.Errorf("unable to create chan backup: %v", err)
	}

	return staticChanBackup, nil
}

// FetchStaticChanBackups will return a plaintext static channel back up for
// all known active/open channels within the passed channel source.
func FetchStaticChanBackups(chanSource LiveChannelSource,
	addrSource AddressSource) ([]Single, error) {

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
		chanBackup, err := assembleChanBackup(addrSource, openChan)
		if err != nil {
			return nil, err
		}

		staticChanBackups = append(staticChanBackups, *chanBackup)
	}

	return staticChanBackups, nil
}
