package lnd

import (
	"fmt"
	"math"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
)

const (
	// mainnetSCBLaunchBlock is the approximate block height of the bitcoin
	// mainnet chain of the date when SCBs first were released in lnd
	// (v0.6.0-beta). The block date is 4/15/2019, 10:54 PM UTC.
	mainnetSCBLaunchBlock = 571800

	// testnetSCBLaunchBlock is the approximate block height of the bitcoin
	// testnet3 chain of the date when SCBs first were released in lnd
	// (v0.6.0-beta). The block date is 4/16/2019, 08:04 AM UTC.
	testnetSCBLaunchBlock = 1489300
)

// chanDBRestorer is an implementation of the chanbackup.ChannelRestorer
// interface that is able to properly map a Single backup, into a
// channeldb.ChannelShell which is required to fully restore a channel. We also
// need the secret key chain in order obtain the prior shachain root so we can
// verify the DLP protocol as initiated by the remote node.
type chanDBRestorer struct {
	db *channeldb.ChannelStateDB

	secretKeys keychain.SecretKeyRing

	chainArb *contractcourt.ChainArbitrator
}

// openChannelShell maps the static channel back up into an open channel
// "shell". We say shell as this doesn't include all the information required
// to continue to use the channel, only the minimal amount of information to
// insert this shell channel back into the database.
func (c *chanDBRestorer) openChannelShell(backup chanbackup.Single) (
	*channeldb.ChannelShell, error) {

	var err error

	// Each of the keys in our local channel config only have their
	// locators populate, so we'll re-derive the raw key now as we'll need
	// it in order to carry out the DLP protocol.
	backup.LocalChanCfg.MultiSigKey, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.MultiSigKey.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive multi sig key: %w",
			err)
	}
	backup.LocalChanCfg.RevocationBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.RevocationBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive revocation key: %w",
			err)
	}
	backup.LocalChanCfg.PaymentBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.PaymentBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive payment key: %w", err)
	}
	backup.LocalChanCfg.DelayBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.DelayBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive delay key: %w", err)
	}
	backup.LocalChanCfg.HtlcBasePoint, err = c.secretKeys.DeriveKey(
		backup.LocalChanCfg.HtlcBasePoint.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive htlc key: %w", err)
	}

	// The shachain root that seeds RevocationProducer for this channel.
	// It currently has two possible formats.
	var revRoot *chainhash.Hash

	// If the PubKey field is non-nil, then this shachain root is using the
	// legacy non-ECDH scheme.
	if backup.ShaChainRootDesc.PubKey != nil {
		ltndLog.Debugf("Using legacy revocation producer format for "+
			"channel point %v", backup.FundingOutpoint)

		// Obtain the private key for the shachain root from the
		// encoded public key.
		privKey, err := c.secretKeys.DerivePrivKey(
			backup.ShaChainRootDesc,
		)
		if err != nil {
			return nil, fmt.Errorf("could not derive private key "+
				"for legacy channel revocation root format: "+
				"%v", err)
		}

		revRoot, err = chainhash.NewHash(privKey.Serialize())
		if err != nil {
			return nil, err
		}
	} else {
		ltndLog.Debugf("Using new ECDH revocation producer format "+
			"for channel point %v", backup.FundingOutpoint)

		// This is the scheme in which the shachain root is derived via
		// an ECDH operation on the private key of ShaChainRootDesc and
		// our public multisig key.
		ecdh, err := c.secretKeys.ECDH(
			backup.ShaChainRootDesc,
			backup.LocalChanCfg.MultiSigKey.PubKey,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to derive shachain "+
				"root: %v", err)
		}

		ch := chainhash.Hash(ecdh)
		revRoot = &ch
	}

	shaChainProducer := shachain.NewRevocationProducer(*revRoot)

	var chanType channeldb.ChannelType
	switch backup.Version {
	case chanbackup.DefaultSingleVersion:
		chanType = channeldb.SingleFunderBit

	case chanbackup.TweaklessCommitVersion:
		chanType = channeldb.SingleFunderTweaklessBit

	case chanbackup.AnchorsCommitVersion:
		chanType = channeldb.AnchorOutputsBit
		chanType |= channeldb.SingleFunderTweaklessBit

	case chanbackup.AnchorsZeroFeeHtlcTxCommitVersion:
		chanType = channeldb.ZeroHtlcTxFeeBit
		chanType |= channeldb.AnchorOutputsBit
		chanType |= channeldb.SingleFunderTweaklessBit

	case chanbackup.ScriptEnforcedLeaseVersion:
		chanType = channeldb.LeaseExpirationBit
		chanType |= channeldb.ZeroHtlcTxFeeBit
		chanType |= channeldb.AnchorOutputsBit
		chanType |= channeldb.SingleFunderTweaklessBit

	case chanbackup.SimpleTaprootVersion:
		chanType = channeldb.ZeroHtlcTxFeeBit
		chanType |= channeldb.AnchorOutputsBit
		chanType |= channeldb.SingleFunderTweaklessBit
		chanType |= channeldb.SimpleTaprootFeatureBit

	case chanbackup.TapscriptRootVersion:
		chanType = channeldb.ZeroHtlcTxFeeBit
		chanType |= channeldb.AnchorOutputsBit
		chanType |= channeldb.SingleFunderTweaklessBit
		chanType |= channeldb.SimpleTaprootFeatureBit
		chanType |= channeldb.TapscriptRootBit

	default:
		return nil, fmt.Errorf("unknown Single version: %w", err)
	}

	ltndLog.Infof("SCB Recovery: created channel shell for ChannelPoint"+
		"(%v), chan_type=%v", backup.FundingOutpoint, chanType)

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
			ThawHeight:              backup.LeaseExpiry,
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
	firstChanHeight := uint32(math.MaxUint32)
	for _, backup := range backups {
		chanShell, err := c.openChannelShell(backup)
		if err != nil {
			return err
		}

		// Find the block height of the earliest channel in this backup.
		chanHeight := chanShell.Chan.ShortChanID().BlockHeight
		if chanHeight != 0 && chanHeight < firstChanHeight {
			firstChanHeight = chanHeight
		}

		channelShells = append(channelShells, chanShell)
	}

	// In case there were only unconfirmed channels, we will have to scan
	// the chain beginning from the launch date of SCBs.
	if firstChanHeight == math.MaxUint32 {
		chainHash := channelShells[0].Chan.ChainHash
		switch {
		case chainHash.IsEqual(chaincfg.MainNetParams.GenesisHash):
			firstChanHeight = mainnetSCBLaunchBlock

		case chainHash.IsEqual(chaincfg.TestNet3Params.GenesisHash):
			firstChanHeight = testnetSCBLaunchBlock

		default:
			// Worst case: We have no height hint and start at
			// block 1. Should only happen for SCBs in regtest
			// and simnet.
			firstChanHeight = 1
		}
	}

	// If there were channels in the backup that were not confirmed at the
	// time of the backup creation, they won't have a block height in the
	// ShortChanID which would lead to an error in the chain watcher.
	// We want to at least set the funding broadcast height that the chain
	// watcher can use instead. We have two possible fallback values for
	// the broadcast height that we are going to try here.
	for _, chanShell := range channelShells {
		channel := chanShell.Chan

		switch {
		// Fallback case 1: This is an unconfirmed channel from an old
		// backup file where we didn't have any workaround in place and
		// the short channel ID is 0:0:0. Best we can do here is set the
		// funding broadcast height to a reasonable value that we
		// determined earlier.
		case channel.ShortChanID().BlockHeight == 0:
			channel.SetBroadcastHeight(firstChanHeight)

		// Fallback case 2: It is extremely unlikely at this point that
		// a channel we are trying to restore has a coinbase funding TX.
		// Therefore we can be quite certain that if the TxIndex is
		// zero but the block height wasn't, it was an unconfirmed
		// channel where we used the BlockHeight to encode the funding
		// TX broadcast height. To not end up with an invalid short
		// channel ID that looks valid, we restore the "original"
		// unconfirmed one here.
		case channel.ShortChannelID.TxIndex == 0:
			broadcastHeight := channel.ShortChannelID.BlockHeight
			channel.SetBroadcastHeight(broadcastHeight)
			channel.ShortChannelID.BlockHeight = 0
		}
	}

	ltndLog.Infof("Inserting %v SCB channel shells into DB",
		len(channelShells))

	// Now that we have all the backups mapped into a series of Singles,
	// we'll insert them all into the database.
	if err := c.db.RestoreChannelShells(channelShells...); err != nil {
		return err
	}

	ltndLog.Infof("Informing chain watchers of new restored channels")

	// Create a slice of channel points.
	chanPoints := make([]wire.OutPoint, 0, len(channelShells))

	// Finally, we'll need to inform the chain arbitrator of these new
	// channels so we'll properly watch for their ultimate closure on chain
	// and sweep them via the DLP.
	for _, restoredChannel := range channelShells {
		err := c.chainArb.WatchNewChannel(restoredChannel.Chan)
		if err != nil {
			return err
		}

		chanPoints = append(
			chanPoints, restoredChannel.Chan.FundingOutpoint,
		)
	}

	// With all the channels restored, we'll now re-send the blockbeat.
	c.chainArb.RedispatchBlockbeat(chanPoints)

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
		ltndLog.Infof("Peer(%x) is already connected, proceeding "+
			"with chan restore", nodePub.SerializeCompressed())
	}

	// For each of the known addresses, we'll attempt to launch a
	// persistent connection to the (pub, addr) pair. In the event that any
	// of them connect, all the other stale requests will be canceled.
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
		err := s.ConnectToPeer(netAddr, true, s.cfg.ConnectionTimeout)

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
