package chanbackup

import (
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnutils"
)

// ChannelRestorer is an interface that allows the Recover method to map the
// set of single channel backups into a set of "channel shells" and store these
// persistently on disk. The channel shell should contain all the information
// needed to execute the data loss recovery protocol once the channel peer is
// connected to.
type ChannelRestorer interface {
	// RestoreChansFromSingles attempts to map the set of single channel
	// backups to channel shells that will be stored persistently. Once
	// these shells have been stored on disk, we'll be able to connect to
	// the channel peer an execute the data loss recovery protocol.
	RestoreChansFromSingles(...Single) error
}

// PeerConnector is an interface that allows the Recover method to connect to
// the target node given the set of possible addresses.
type PeerConnector interface {
	// ConnectPeer attempts to connect to the target node at the set of
	// available addresses. Once this method returns with a non-nil error,
	// the connector should attempt to persistently connect to the target
	// peer in the background as a persistent attempt.
	ConnectPeer(node *btcec.PublicKey, addrs []net.Addr) error
}

// Recover attempts to recover the static channel state from a set of static
// channel backups. If successfully, the database will be populated with a
// series of "shell" channels. These "shell" channels cannot be used to operate
// the channel as normal, but instead are meant to be used to enter the data
// loss recovery phase, and recover the settled funds within
// the channel. In addition a LinkNode will be created for each new peer as
// well, in order to expose the addressing information required to locate to
// and connect to each peer in order to initiate the recovery protocol.
// The number of channels that were successfully restored is returned.
func Recover(backups []Single, restorer ChannelRestorer,
	peerConnector PeerConnector) (int, error) {

	var numRestored int
	for i, backup := range backups {
		log.Infof("Restoring ChannelPoint(%v) to disk: ",
			backup.FundingOutpoint)

		err := restorer.RestoreChansFromSingles(backup)

		// If a channel is already present in the channel DB, we can
		// just continue. No reason to fail a whole set of multi backups
		// for example. This allows resume of a restore in case another
		// error happens.
		if err == channeldb.ErrChanAlreadyExists {
			continue
		}
		if err != nil {
			return numRestored, err
		}

		numRestored++
		log.Infof("Attempting to connect to node=%x (addrs=%v) to "+
			"restore ChannelPoint(%v)",
			backup.RemoteNodePub.SerializeCompressed(),
			lnutils.SpewLogClosure(backups[i].Addresses),
			backup.FundingOutpoint)

		err = peerConnector.ConnectPeer(
			backup.RemoteNodePub, backup.Addresses,
		)
		if err != nil {
			return numRestored, err
		}

		// TODO(roasbeef): to handle case where node has changed addrs,
		// need to subscribe to new updates for target node pub to
		// attempt to connect to other addrs
		//
		//  * just to to fresh w/ call to node addrs and de-dup?
	}

	return numRestored, nil
}

// TODO(roasbeef): more specific keychain interface?

// UnpackAndRecoverSingles is a one-shot method, that given a set of packed
// single channel backups, will restore the channel state to a channel shell,
// and also reach out to connect to any of the known node addresses for that
// channel. It is assumes that after this method exists, if a connection was
// established, then the PeerConnector will continue to attempt to re-establish
// a persistent connection in the background. The number of channels that were
// successfully restored is returned.
func UnpackAndRecoverSingles(singles PackedSingles,
	keyChain keychain.KeyRing, restorer ChannelRestorer,
	peerConnector PeerConnector) (int, error) {

	chanBackups, err := singles.Unpack(keyChain)
	if err != nil {
		return 0, err
	}

	return Recover(chanBackups, restorer, peerConnector)
}

// UnpackAndRecoverMulti is a one-shot method, that given a set of packed
// multi-channel backups, will restore the channel states to channel shells,
// and also reach out to connect to any of the known node addresses for that
// channel. It is assumes that after this method exists, if a connection was
// established, then the PeerConnector will continue to attempt to re-establish
// a persistent connection in the background. The number of channels that were
// successfully restored is returned.
func UnpackAndRecoverMulti(packedMulti PackedMulti,
	keyChain keychain.KeyRing, restorer ChannelRestorer,
	peerConnector PeerConnector) (int, error) {

	chanBackups, err := packedMulti.Unpack(keyChain)
	if err != nil {
		return 0, err
	}

	return Recover(chanBackups.StaticBackups, restorer, peerConnector)
}
