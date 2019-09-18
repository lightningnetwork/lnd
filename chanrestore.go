package lnd

import (
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

// chanDBRestorer is an implementation of the chanbackup.ChannelRestorer
// interface that is able to properly map a Single backup, into a
// channeldb.ChannelShell which is required to fully restore a channel. We also
// need the secret key chain in order obtain the prior shachain root so we can
// verify the DLP protocol as initiated by the remote node.
type chanDBRestorer struct {
	db *channeldb.DB

	secretKeys keychain.SecretKeyRing

	chainArb *contractcourt.ChainArbitrator
}

// openChannelShell maps the static channel back up into an open channel
// "shell". We say shell as this doesn't include all the information required
// to continue to use the channel, only the minimal amount of information to
// insert this shell channel back into the database.
func (c *chanDBRestorer) openChannelShell(backup chanbackup.Single) (
	*channeldb.ChannelShell, error) {

	// First, we'll also need to obtain the private key for the shachain
	// root from the encoded public key.
	//
	// TODO(roasbeef): now adds req for hardware signers to impl
	// shachain...
	privKey, err := c.secretKeys.DerivePrivKey(backup.ShaChainRootDesc)
	if err != nil {
		return nil, fmt.Errorf("unable to derive shachain root key: %v", err)
	}
	revRoot, err := chainhash.NewHash(privKey.Serialize())
	if err != nil {
		return nil, err
	}
	shaChainProducer := shachain.NewRevocationProducer(*revRoot)

	// Each of the keys in our local channel config only have their
	// locators populate, so we'll re-derive the raw key now as we'll need
	// it in order to carry out the DLP protocol.
	backup.LocalChanCfg.MultiSigKey, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.MultiSigKey.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive multi sig key: %v", err)
	}
	backup.LocalChanCfg.RevocationBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.RevocationBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive revocation key: %v", err)
	}
	backup.LocalChanCfg.PaymentBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.PaymentBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive payment key: %v", err)
	}
	backup.LocalChanCfg.DelayBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.DelayBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive delay key: %v", err)
	}
	backup.LocalChanCfg.HtlcBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.HtlcBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive htlc key: %v", err)
	}

	var chanType channeldb.ChannelType
	switch backup.Version {

	case chanbackup.DefaultSingleVersion:
		chanType = channeldb.SingleFunder

	case chanbackup.TweaklessCommitVersion:
		chanType = channeldb.SingleFunderTweakless

	default:
		return nil, fmt.Errorf("unknown Single version: %v", err)
	}

	chanShell := channeldb.ChannelShell{
		NodeAddrs: backup.Addresses,
		Chan: &channeldb.OpenChannel{
			ChanType:                chanType,
			ChainHash:               backup.ChainHash,
			IsInitiator:             backup.IsInitiator,
			Capacity:                backup.Capacity,
			FundingOutpoint:         backup.FundingOutpoint,
			ShortChannelID:          backup.ShortChannelID,
			IdentityPub:             backup.RemoteNodePub,
			IsPending:               false,
			LocalChanCfg:            backup.LocalChanCfg,
			RemoteChanCfg:           backup.RemoteChanCfg,
			RemoteCurrentRevocation: backup.RemoteNodePub,
			RevocationStore:         shachain.NewRevocationStore(),
			RevocationProducer:      shaChainProducer,
		},
	}

	return &chanShell, nil
}

// RestoreChansFromSingles attempts to map the set of single channel backups to
// channel shells that will be stored persistently. Once these shells have been
// stored on disk, we'll be able to connect to the channel peer an execute the
// data loss recovery protocol.
//
// NOTE: Part of the chanbackup.ChannelRestorer interface.
func (c *chanDBRestorer) RestoreChansFromSingles(backups ...chanbackup.Single) error {
	channelShells := make([]*channeldb.ChannelShell, 0, len(backups))
	for _, backup := range backups {
		chanShell, err := c.openChannelShell(backup)
		if err != nil {
			return err
		}

		channelShells = append(channelShells, chanShell)
	}

	ltndLog.Infof("Inserting %v SCB channel shells into DB",
		len(channelShells))

	// Now that we have all the backups mapped into a series of Singles,
	// we'll insert them all into the database.
	if err := c.db.RestoreChannelShells(channelShells...); err != nil {
		return err
	}

	ltndLog.Infof("Informing chain watchers of new restored channels")

	// Finally, we'll need to inform the chain arbitrator of these new
	// channels so we'll properly watch for their ultimate closure on chain
	// and sweep them via the DLP.
	for _, restoredChannel := range channelShells {
		err := c.chainArb.WatchNewChannel(restoredChannel.Chan)
		if err != nil {
			return err
		}
	}

	return nil
}

// A compile-time constraint to ensure chanDBRestorer implements
// chanbackup.ChannelRestorer.
var _ chanbackup.ChannelRestorer = (*chanDBRestorer)(nil)

// ConnectPeer attempts to connect to the target node at the set of available
// addresses. Once this method returns with a non-nil error, the connector
// should attempt to persistently connect to the target peer in the background
// as a persistent attempt.
//
// NOTE: Part of the chanbackup.PeerConnector interface.
func (s *server) ConnectPeer(nodePub *btcec.PublicKey, addrs []net.Addr) error {
	// Before we connect to the remote peer, we'll remove any connections
	// to ensure the new connection is created after this new link/channel
	// is known.
	if err := s.DisconnectPeer(nodePub); err != nil {
		ltndLog.Infof("Peer(%v) is already connected, proceeding "+
			"with chan restore", nodePub.SerializeCompressed())
	}

	// For each of the known addresses, we'll attempt to launch a
	// persistent connection to the (pub, addr) pair. In the event that any
	// of them connect, all the other stale requests will be cancelled.
	for _, addr := range addrs {
		netAddr := &lnwire.NetAddress{
			IdentityKey: nodePub,
			Address:     addr,
		}

		ltndLog.Infof("Attempting to connect to %v for SCB restore "+
			"DLP", netAddr)

		// Attempt to connect to the peer using this full address. If
		// we're unable to connect to them, then we'll try the next
		// address in place of it.
		err := s.ConnectToPeer(netAddr, true)

		// If we're already connected to this peer, then we don't
		// consider this an error, so we'll exit here.
		if _, ok := err.(*errPeerAlreadyConnected); ok {
			return nil

		} else if err != nil {
			// Otherwise, something else happened, so we'll try the
			// next address.
			ltndLog.Errorf("unable to connect to %v to "+
				"complete SCB restore: %v", netAddr, err)
			continue
		}

		// If we connected no problem, then we can exit early as our
		// job here is done.
		return nil
	}

	return fmt.Errorf("unable to connect to peer %x for SCB restore",
		nodePub.SerializeCompressed())
}
