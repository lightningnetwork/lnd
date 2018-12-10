package main

import (
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channeldb"
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
		return nil, nil
	}
	revRoot, err := chainhash.NewHash(privKey.Serialize())
	if err != nil {
		return nil, err
	}
	shaChainProducer := shachain.NewRevocationProducer(*revRoot)

	chanShell := channeldb.ChannelShell{
		NodeAddrs: backup.Addresses,
		Chan: &channeldb.OpenChannel{
			ChainHash:       backup.ChainHash,
			FundingOutpoint: backup.FundingOutpoint,
			ShortChannelID:  backup.ShortChannelID,
			IdentityPub:     backup.RemoteNodePub,
			IsPending:       false,
			LocalChanCfg: channeldb.ChannelConfig{
				CsvDelay: backup.CsvDelay,
				PaymentBasePoint: keychain.KeyDescriptor{
					KeyLocator: backup.PaymentBasePoint,
				},
			},
			// We'll set this to a dummy value, as we can't
			// actually validate it ourselves.
			RemoteCurrentRevocation: backup.RemoteNodePub,
			RevocationStore:         shachain.NewRevocationStore(),
			RevocationProducer:      shaChainProducer,
		},
	}

	// TODO(roasbeef): move this mapping elsewhere?

	// When we make a channel, we mark that the channel has been restored,
	// this will signal to other sub-systems to not attempt to use the
	// channel as if it was a regular one.
	chanStatus := channeldb.ChanStatusDefault |
		channeldb.ChanStatusRestored

	chanShell.Chan.ApplyChanStatus(chanStatus)

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

	return c.db.RestoreChannelShells(channelShells...)
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
	// For each of the known addresses, we'll attempt to launch a
	// persistent connection to the (pub, addr) pair. In the event that any
	// of them connect, all the other stale requests will be cancelled.
	for _, addr := range addrs {
		netAddr := &lnwire.NetAddress{
			IdentityKey: nodePub,
			Address:     addr,
		}
		if err := s.ConnectToPeer(netAddr, true); err != nil {
			return err
		}
	}

	return nil
}
