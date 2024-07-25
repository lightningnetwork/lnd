package chanbackup

import (
	"errors"
	"fmt"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/shachain"
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
func Recover(backups []Single, restorer ChannelRestorer,
	peerConnector PeerConnector) error {

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
			return err
		}

		log.Infof("Attempting to connect to node=%x (addrs=%v) to "+
			"restore ChannelPoint(%v)",
			backup.RemoteNodePub.SerializeCompressed(),
			newLogClosure(func() string {
				return spew.Sdump(backups[i].Addresses)
			}), backup.FundingOutpoint)

		err = peerConnector.ConnectPeer(
			backup.RemoteNodePub, backup.Addresses,
		)
		if err != nil {
			return err
		}

		// TODO(roasbeef): to handle case where node has changed addrs,
		// need to subscribe to new updates for target node pub to
		// attempt to connect to other addrs
		//
		//  * just to to fresh w/ call to node addrs and de-dup?
	}

	return nil
}

// TODO(roasbeef): more specific keychain interface?

// UnpackAndRecoverSingles is a one-shot method, that given a set of packed
// single channel backups, will restore the channel state to a channel shell,
// and also reach out to connect to any of the known node addresses for that
// channel. It is assumes that after this method exists, if a connection we
// able to be established, then then PeerConnector will continue to attempt to
// re-establish a persistent connection in the background.
func UnpackAndRecoverSingles(singles PackedSingles,
	keyChain keychain.KeyRing, restorer ChannelRestorer,
	peerConnector PeerConnector) error {

	chanBackups, err := singles.Unpack(keyChain)
	if err != nil {
		return err
	}

	return Recover(chanBackups, restorer, peerConnector)
}

// UnpackAndRecoverMulti is a one-shot method, that given a set of packed
// multi-channel backups, will restore the channel states to channel shells,
// and also reach out to connect to any of the known node addresses for that
// channel. It is assumes that after this method exists, if a connection we
// able to be established, then then PeerConnector will continue to attempt to
// re-establish a persistent connection in the background.
func UnpackAndRecoverMulti(packedMulti PackedMulti,
	keyChain keychain.KeyRing, restorer ChannelRestorer,
	peerConnector PeerConnector) error {

	chanBackups, err := packedMulti.Unpack(keyChain)
	if err != nil {
		return err
	}

	return Recover(chanBackups.StaticBackups, restorer, peerConnector)
}

// SignCloseTx produces a signed commit tx from a channel backup.
func SignCloseTx(s Single, keyRing keychain.KeyRing, ecdher keychain.ECDHRing,
	signer input.Signer) (*wire.MsgTx, error) {

	if s.CloseTxInputs == nil {
		return nil, errors.New("channel backup does not have data " +
			"needed to sign force close tx")
	}

	// Each of the keys in our local channel config only have their
	// locators populate, so we'll re-derive the raw key now.
	localMultiSigKey, err := keyRing.DeriveKey(
		s.LocalChanCfg.MultiSigKey.KeyLocator,
	)
	if err != nil {
		return nil, fmt.Errorf("unable to derive multisig key: %w", err)
	}

	signDesc, err := createSignDesc(
		localMultiSigKey, s.RemoteChanCfg.MultiSigKey.PubKey,
		s.Version, s.Capacity,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signDesc: %w", err)
	}

	inputs := lnwallet.SignedCommitTxInputs{
		CommitTx:  s.CloseTxInputs.CommitTx,
		CommitSig: s.CloseTxInputs.CommitSig,
		OurKey:    localMultiSigKey,
		TheirKey:  s.RemoteChanCfg.MultiSigKey,
		SignDesc:  signDesc,
	}

	if s.Version.IsTaproot() {
		producer, err := createTaprootNonceProducer(
			s.ShaChainRootDesc, localMultiSigKey.PubKey, ecdher,
		)
		if err != nil {
			return nil, err
		}
		inputs.Taproot = &lnwallet.TaprootSignedCommitTxInputs{
			CommitHeight:         s.CloseTxInputs.CommitHeight,
			TaprootNonceProducer: producer,
		}
	}

	return lnwallet.GetSignedCommitTx(inputs, signer)
}

// createSignDesc creates SignDescriptor from local and remote keys,
// backup version and capacity.
// See LightningChannel.createSignDesc on how signDesc is produced.
func createSignDesc(localMultiSigKey keychain.KeyDescriptor,
	remoteKey *btcec.PublicKey, version SingleBackupVersion,
	capacity btcutil.Amount) (*input.SignDescriptor, error) {

	var fundingPkScript, multiSigScript []byte

	localKey := localMultiSigKey.PubKey

	var err error
	if version.IsTaproot() {
		fundingPkScript, _, err = input.GenTaprootFundingScript(
			localKey, remoteKey, int64(capacity),
		)
		if err != nil {
			return nil, err
		}
	} else {
		multiSigScript, err = input.GenMultiSigScript(
			localKey.SerializeCompressed(),
			remoteKey.SerializeCompressed(),
		)
		if err != nil {
			return nil, err
		}

		fundingPkScript, err = input.WitnessScriptHash(multiSigScript)
		if err != nil {
			return nil, err
		}
	}

	return &input.SignDescriptor{
		KeyDesc:       localMultiSigKey,
		WitnessScript: multiSigScript,
		Output: &wire.TxOut{
			PkScript: fundingPkScript,
			Value:    int64(capacity),
		},
		HashType: txscript.SigHashAll,
		PrevOutputFetcher: txscript.NewCannedPrevOutputFetcher(
			fundingPkScript, int64(capacity),
		),
		InputIndex: 0,
	}, nil
}

// createTaprootNonceProducer makes taproot nonce producer from a
// ShaChainRootDesc and our public multisig key.
func createTaprootNonceProducer(shaChainRootDesc keychain.KeyDescriptor,
	localKey *btcec.PublicKey, ecdher keychain.ECDHRing) (shachain.Producer,
	error) {

	if shaChainRootDesc.PubKey != nil {
		return nil, errors.New("taproot channels always use ECDH, " +
			"but legacy ShaChainRootDesc with pubkey found")
	}

	// This is the scheme in which the shachain root is derived via an ECDH
	// operation on the private key of ShaChainRootDesc and our public
	// multisig key.
	ecdh, err := ecdher.ECDH(shaChainRootDesc, localKey)
	if err != nil {
		return nil, fmt.Errorf("ecdh failed: %w", err)
	}

	// The shachain root that seeds RevocationProducer for this channel.
	revRoot := chainhash.Hash(ecdh)

	revocationProducer := shachain.NewRevocationProducer(revRoot)

	return channeldb.DeriveMusig2Shachain(revocationProducer)
}
