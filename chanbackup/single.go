package chanbackup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SingleBackupVersion denotes the version of the single static channel backup.
// Based on this version, we know how to pack/unpack serialized versions of the
// backup.
type SingleBackupVersion byte

const (
	// DefaultSingleVersion is the default version of the single channel
	// backup. The serialized version of this static channel backup is
	// simply: version || SCB. Where SCB is the known format of the
	// version.
	DefaultSingleVersion = 0

	// TweaklessCommitVersion is the second SCB version. This version
	// implicitly denotes that this channel uses the new tweakless commit
	// format.
	TweaklessCommitVersion = 1

	// AnchorsCommitVersion is the third SCB version. This version
	// implicitly denotes that this channel uses the new anchor commitment
	// format.
	AnchorsCommitVersion = 2

	// AnchorsZeroFeeHtlcTxCommitVersion is a version that denotes this
	// channel is using the zero-fee second-level anchor commitment format.
	AnchorsZeroFeeHtlcTxCommitVersion = 3

	// ScriptEnforcedLeaseVersion is a version that denotes this channel is
	// using the zero-fee second-level anchor commitment format along with
	// an additional CLTV requirement of the channel lease maturity on any
	// commitment and HTLC outputs that pay directly to the channel
	// initiator.
	ScriptEnforcedLeaseVersion = 4

	// SimpleTaprootVersion is a version that denotes this channel is using
	// the musig2 based taproot commitment format.
	SimpleTaprootVersion = 5

	// TapscriptRootVersion is a version that denotes this is a MuSig2
	// channel with a top level tapscript commitment.
	TapscriptRootVersion = 6

	// closeTxVersionMask is the byte mask used that is ORed to version byte
	// on wire indicating that the backup has CloseTxInputs.
	closeTxVersionMask = 1 << 7
)

// Encode returns encoding of the version to put into channel backup.
// Argument "closeTx" specifies if the backup includes force close transaction.
func (v SingleBackupVersion) Encode(closeTx bool) byte {
	encoded := byte(v)

	// If the backup includes closing transaction, set this bit in the
	// encoded version.
	if closeTx {
		encoded |= closeTxVersionMask
	}

	return encoded
}

// DecodeVersion decodes the encoding of the version from a channel backup.
// It returns the version and if the backup includes the force close tx.
func DecodeVersion(encoded byte) (SingleBackupVersion, bool) {
	// Find if it has a closing transaction by inspecting the bit.
	closeTx := (encoded & closeTxVersionMask) != 0

	// The version byte also encodes the closeTxVersion feature, so we
	// extract it here and return it separately to the backup version.
	version := SingleBackupVersion(encoded &^ closeTxVersionMask)

	return version, closeTx
}

// IsTaproot returns if this is a backup of a taproot channel. This will also be
// true for simple taproot overlay channels when a version is added.
func (v SingleBackupVersion) IsTaproot() bool {
	return v == SimpleTaprootVersion || v == TapscriptRootVersion
}

// HasTapscriptRoot returns true if the channel is using a top level tapscript
// root commitment.
func (v SingleBackupVersion) HasTapscriptRoot() bool {
	return v == TapscriptRootVersion
}

// Single is a static description of an existing channel that can be used for
// the purposes of backing up. The fields in this struct allow a node to
// recover the settled funds within a channel in the case of partial or
// complete data loss. We provide the network address that we last used to
// connect to the peer as well, in case the node stops advertising the IP on
// the network for whatever reason.
//
// TODO(roasbeef): suffix version into struct?
type Single struct {
	// Version is the version that should be observed when attempting to
	// pack the single backup.
	Version SingleBackupVersion

	// IsInitiator is true if we were the initiator of the channel, and
	// false otherwise. We'll need to know this information in order to
	// properly re-derive the state hint information.
	IsInitiator bool

	// ChainHash is a hash which represents the blockchain that this
	// channel will be opened within. This value is typically the genesis
	// hash. In the case that the original chain went through a contentious
	// hard-fork, then this value will be tweaked using the unique fork
	// point on each branch.
	ChainHash chainhash.Hash

	// FundingOutpoint is the outpoint of the final funding transaction.
	// This value uniquely and globally identities the channel within the
	// target blockchain as specified by the chain hash parameter.
	FundingOutpoint wire.OutPoint

	// ShortChannelID encodes the exact location in the chain in which the
	// channel was initially confirmed. This includes: the block height,
	// transaction index, and the output within the target transaction.
	// Channels that were not confirmed at the time of backup creation will
	// have the funding TX broadcast height set as their block height in
	// the ShortChannelID.
	ShortChannelID lnwire.ShortChannelID

	// RemoteNodePub is the identity public key of the remote node this
	// channel has been established with.
	RemoteNodePub *btcec.PublicKey

	// Addresses is a list of IP address in which either we were able to
	// reach the node over in the past, OR we received an incoming
	// authenticated connection for the stored identity public key.
	Addresses []net.Addr

	// Capacity is the size of the original channel.
	Capacity btcutil.Amount

	// LocalChanCfg is our local channel configuration. It contains all the
	// information we need to re-derive the keys we used within the
	// channel. Most importantly, it allows to derive the base public
	// that's used to deriving the key used within the non-delayed
	// pay-to-self output on the commitment transaction for a node. With
	// this information, we can re-derive the private key needed to sweep
	// the funds on-chain.
	//
	// NOTE: Of the items in the ChannelConstraints, we only write the CSV
	// delay.
	LocalChanCfg channeldb.ChannelConfig

	// RemoteChanCfg is the remote channel confirmation. We store this as
	// well since we'll need some of their keys to re-derive things like
	// the state hint obfuscator which will allow us to recognize the state
	// their broadcast on chain.
	//
	// NOTE: Of the items in the ChannelConstraints, we only write the CSV
	// delay.
	RemoteChanCfg channeldb.ChannelConfig

	// ShaChainRootDesc describes how to derive the private key that was
	// used as the shachain root for this channel.
	ShaChainRootDesc keychain.KeyDescriptor

	// LeaseExpiry represents the absolute expiration as a height of the
	// chain of a channel lease that is applied to every output that pays
	// directly to the channel initiator in addition to the usual CSV
	// requirement.
	//
	// NOTE: This field will only be present for the following versions:
	//
	// - ScriptEnforcedLeaseVersion
	LeaseExpiry uint32

	// CloseTxInputs contains data needed to produce a force close tx
	// using for example the "chantools scbforceclose" command.
	//
	// The field is optional.
	CloseTxInputs fn.Option[CloseTxInputs]
}

// CloseTxInputs contains data needed to produce a force close transaction
// using for example the "chantools scbforceclose" command.
type CloseTxInputs struct {
	// CommitTx is the latest version of the commitment state, broadcast
	// able by us, but not signed. It can be signed by for example the
	// "chantools scbforceclose" command.
	CommitTx *wire.MsgTx

	// CommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above. This is the
	// signature signed by the remote party for our version of the
	// commitment transactions.
	CommitSig []byte

	// CommitHeight is the update number that this ChannelDelta represents
	// the total number of commitment updates to this point. This can be
	// viewed as sort of a "commitment height" as this number is
	// monotonically increasing.
	//
	// This field is filled only for taproot channels.
	CommitHeight uint64

	// TapscriptRoot is the root of the tapscript tree that will be used to
	// create the funding output. This is an optional field that should
	// only be set for overlay taproot channels (HasTapscriptRoot).
	TapscriptRoot fn.Option[chainhash.Hash]
}

// NewSingle creates a new static channel backup based on an existing open
// channel. We also pass in the set of addresses that we used in the past to
// connect to the channel peer. If possible, we include the data needed to
// produce a force close transaction from the most recent state using externally
// provided private key.
func NewSingle(channel *channeldb.OpenChannel,
	nodeAddrs []net.Addr) Single {

	var shaChainRootDesc keychain.KeyDescriptor

	// If the channel has a populated RevocationKeyLocator, then we can
	// just store that instead of the public key.
	if channel.RevocationKeyLocator.Family == keychain.KeyFamilyRevocationRoot {
		shaChainRootDesc = keychain.KeyDescriptor{
			KeyLocator: channel.RevocationKeyLocator,
		}
	} else {
		// If the RevocationKeyLocator is not populated, then we'll need
		// to obtain a public point for the shachain root and store that.
		// This is the legacy scheme.
		var b bytes.Buffer
		_ = channel.RevocationProducer.Encode(&b) // Can't return an error.

		// Once we have the root, we'll make a public key from it, such that
		// the backups plaintext don't carry any private information. When
		// we go to recover, we'll present this in order to derive the
		// private key.
		_, shaChainPoint := btcec.PrivKeyFromBytes(b.Bytes())

		shaChainRootDesc = keychain.KeyDescriptor{
			PubKey: shaChainPoint,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyRevocationRoot,
			},
		}
	}

	// If a channel is unconfirmed, the block height of the ShortChannelID
	// is zero. This will lead to problems when trying to restore that
	// channel as the spend notifier would get a height hint of zero.
	// To work around that problem, we add the channel broadcast height
	// to the channel ID so we can use that as height hint on restore.
	chanID := channel.ShortChanID()
	if chanID.BlockHeight == 0 {
		chanID.BlockHeight = channel.BroadcastHeight()
	}

	// If this is a zero-conf channel, we'll need to have separate logic
	// depending on whether it's confirmed or not. This is because the
	// ShortChanID is an alias.
	if channel.IsZeroConf() {
		// If the channel is confirmed, we'll use the confirmed SCID.
		if channel.ZeroConfConfirmed() {
			chanID = channel.ZeroConfRealScid()
		} else {
			// Else if the zero-conf channel is unconfirmed, we'll
			// need to use the broadcast height and zero out the
			// TxIndex and TxPosition fields. This is so
			// openChannelShell works properly.
			chanID.BlockHeight = channel.BroadcastHeight()
			chanID.TxIndex = 0
			chanID.TxPosition = 0
		}
	}

	single := Single{
		IsInitiator:      channel.IsInitiator,
		ChainHash:        channel.ChainHash,
		FundingOutpoint:  channel.FundingOutpoint,
		ShortChannelID:   chanID,
		RemoteNodePub:    channel.IdentityPub,
		Addresses:        nodeAddrs,
		Capacity:         channel.Capacity,
		LocalChanCfg:     channel.LocalChanCfg,
		RemoteChanCfg:    channel.RemoteChanCfg,
		ShaChainRootDesc: shaChainRootDesc,
	}

	switch {
	case channel.ChanType.IsTaproot():
		if channel.ChanType.HasTapscriptRoot() {
			single.Version = TapscriptRootVersion
		} else {
			single.Version = SimpleTaprootVersion
		}

	case channel.ChanType.HasLeaseExpiration():
		single.Version = ScriptEnforcedLeaseVersion
		single.LeaseExpiry = channel.ThawHeight

	case channel.ChanType.ZeroHtlcTxFee():
		single.Version = AnchorsZeroFeeHtlcTxCommitVersion

	case channel.ChanType.HasAnchors():
		single.Version = AnchorsCommitVersion

	case channel.ChanType.IsTweakless():
		single.Version = TweaklessCommitVersion

	default:
		single.Version = DefaultSingleVersion
	}

	// Include unsigned force-close transaction for the most recent channel
	// state as well as the data needed to produce the signature, given the
	// private key is provided separately.
	single.CloseTxInputs = buildCloseTxInputs(channel)

	return single
}

// errEmptyTapscriptRoot is returned by Serialize if field TapscriptRoot is
// empty, when it should be filled according to the channel version.
var errEmptyTapscriptRoot = errors.New("field TapscriptRoot is not filled")

// Serialize attempts to write out the serialized version of the target
// StaticChannelBackup into the passed io.Writer.
func (s *Single) Serialize(w io.Writer) error {
	// Check to ensure that we'll only attempt to serialize a version that
	// we're aware of.
	switch s.Version {
	case DefaultSingleVersion:
	case TweaklessCommitVersion:
	case AnchorsCommitVersion:
	case AnchorsZeroFeeHtlcTxCommitVersion:
	case ScriptEnforcedLeaseVersion:
	case SimpleTaprootVersion:
	case TapscriptRootVersion:
	default:
		return fmt.Errorf("unable to serialize w/ unknown "+
			"version: %v", s.Version)
	}

	// If the sha chain root has specified a public key (which is
	// optional), then we'll encode it now.
	var shaChainPub [33]byte
	if s.ShaChainRootDesc.PubKey != nil {
		copy(
			shaChainPub[:],
			s.ShaChainRootDesc.PubKey.SerializeCompressed(),
		)
	}

	// First we gather the SCB as is into a temporary buffer so we can
	// determine the total length. Before we write out the serialized SCB,
	// we write the length which allows us to skip any Singles that we
	// don't know of when decoding a multi.
	var singleBytes bytes.Buffer
	if err := lnwire.WriteElements(
		&singleBytes,
		s.IsInitiator,
		s.ChainHash[:],
		s.FundingOutpoint,
		s.ShortChannelID,
		s.RemoteNodePub,
		s.Addresses,
		s.Capacity,

		s.LocalChanCfg.CsvDelay,

		// We only need to write out the KeyLocator portion of the
		// local channel config.
		uint32(s.LocalChanCfg.MultiSigKey.Family),
		s.LocalChanCfg.MultiSigKey.Index,
		uint32(s.LocalChanCfg.RevocationBasePoint.Family),
		s.LocalChanCfg.RevocationBasePoint.Index,
		uint32(s.LocalChanCfg.PaymentBasePoint.Family),
		s.LocalChanCfg.PaymentBasePoint.Index,
		uint32(s.LocalChanCfg.DelayBasePoint.Family),
		s.LocalChanCfg.DelayBasePoint.Index,
		uint32(s.LocalChanCfg.HtlcBasePoint.Family),
		s.LocalChanCfg.HtlcBasePoint.Index,

		s.RemoteChanCfg.CsvDelay,

		// We only need to write out the raw pubkey for the remote
		// channel config.
		s.RemoteChanCfg.MultiSigKey.PubKey,
		s.RemoteChanCfg.RevocationBasePoint.PubKey,
		s.RemoteChanCfg.PaymentBasePoint.PubKey,
		s.RemoteChanCfg.DelayBasePoint.PubKey,
		s.RemoteChanCfg.HtlcBasePoint.PubKey,

		shaChainPub[:],
		uint32(s.ShaChainRootDesc.KeyLocator.Family),
		s.ShaChainRootDesc.KeyLocator.Index,
	); err != nil {
		return err
	}
	if s.Version == ScriptEnforcedLeaseVersion {
		err := lnwire.WriteElements(&singleBytes, s.LeaseExpiry)
		if err != nil {
			return err
		}
	}

	// Encode version enum and hasCloseTx flag to version byte.
	version := s.Version.Encode(s.CloseTxInputs.IsSome())

	// Serialize CloseTxInputs if it is provided. Fill err if it fails.
	err := fn.MapOptionZ(s.CloseTxInputs, func(inputs CloseTxInputs) error {
		err := inputs.CommitTx.Serialize(&singleBytes)
		if err != nil {
			return err
		}

		err = lnwire.WriteElements(
			&singleBytes,
			uint16(len(inputs.CommitSig)), inputs.CommitSig,
		)
		if err != nil {
			return err
		}

		if !s.Version.IsTaproot() {
			return nil
		}

		// Write fields needed for taproot channels.
		err = lnwire.WriteElements(
			&singleBytes, inputs.CommitHeight,
		)
		if err != nil {
			return err
		}

		if s.Version.HasTapscriptRoot() {
			opt := inputs.TapscriptRoot
			var tapscriptRoot chainhash.Hash
			tapscriptRoot, err = opt.UnwrapOrErr(
				errEmptyTapscriptRoot,
			)
			if err != nil {
				return err
			}

			err = lnwire.WriteElements(
				&singleBytes, tapscriptRoot[:],
			)
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to encode CloseTxInputs: %w", err)
	}

	// TODO(yy): remove the type assertion when we finished refactoring db
	// into using write buffer.
	buf, ok := w.(*bytes.Buffer)
	if !ok {
		return fmt.Errorf("expect io.Writer to be *bytes.Buffer")
	}

	return lnwire.WriteElements(
		buf,
		version,
		uint16(len(singleBytes.Bytes())),
		singleBytes.Bytes(),
	)
}

// PackToWriter is similar to the Serialize method, but takes the operation a
// step further by encryption the raw bytes of the static channel back up. For
// encryption we use the chacah20poly1305 AEAD cipher with a 24 byte nonce and
// 32-byte key size. We use a 24-byte nonce, as we can't ensure that we have a
// global counter to use as a sequence number for nonces, and want to ensure
// that we're able to decrypt these blobs without any additional context. We
// derive the key that we use for encryption via a SHA2 operation of the with
// the golden keychain.KeyFamilyBaseEncryption base encryption key.  We then
// take the serialized resulting shared secret point, and hash it using sha256
// to obtain the key that we'll use for encryption. When using the AEAD, we
// pass the nonce as associated data such that we'll be able to package the two
// together for storage. Before writing out the encrypted payload, we prepend
// the nonce to the final blob.
func (s *Single) PackToWriter(w io.Writer, keyRing keychain.KeyRing) error {
	// First, we'll serialize the SCB (StaticChannelBackup) into a
	// temporary buffer so we can store it in a temporary place before we
	// go to encrypt the entire thing.
	var rawBytes bytes.Buffer
	if err := s.Serialize(&rawBytes); err != nil {
		return err
	}

	// Finally, we'll encrypt the raw serialized SCB (using the nonce as
	// associated data), and write out the ciphertext prepend with the
	// nonce that we used to the passed io.Reader.
	e, err := lnencrypt.KeyRingEncrypter(keyRing)
	if err != nil {
		return fmt.Errorf("unable to generate encrypt key %w", err)
	}
	return e.EncryptPayloadToWriter(rawBytes.Bytes(), w)
}

// readLocalKeyDesc reads a KeyDescriptor encoded within an unpacked Single.
// For local KeyDescs, we only write out the KeyLocator information as we can
// re-derive the pubkey from it.
func readLocalKeyDesc(r io.Reader) (keychain.KeyDescriptor, error) {
	var keyDesc keychain.KeyDescriptor

	var keyFam uint32
	if err := lnwire.ReadElements(r, &keyFam); err != nil {
		return keyDesc, err
	}
	keyDesc.Family = keychain.KeyFamily(keyFam)

	if err := lnwire.ReadElements(r, &keyDesc.Index); err != nil {
		return keyDesc, err
	}

	return keyDesc, nil
}

// readRemoteKeyDesc reads a remote KeyDescriptor encoded within an unpacked
// Single. For remote KeyDescs, we write out only the PubKey since we don't
// actually have the KeyLocator data.
func readRemoteKeyDesc(r io.Reader) (keychain.KeyDescriptor, error) {
	var (
		keyDesc keychain.KeyDescriptor
		pub     [33]byte
	)

	_, err := io.ReadFull(r, pub[:])
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	keyDesc.PubKey, err = btcec.ParsePubKey(pub[:])
	if err != nil {
		return keychain.KeyDescriptor{}, err
	}

	return keyDesc, nil
}

// Deserialize attempts to read the raw plaintext serialized SCB from the
// passed io.Reader. If the method is successful, then the target
// StaticChannelBackup will be fully populated.
func (s *Single) Deserialize(r io.Reader) error {
	// First, we'll need to read the version of this single-back up so we
	// can know how to unpack each of the SCB.
	var version byte
	err := lnwire.ReadElements(r, &version)
	if err != nil {
		return err
	}

	// Decode version byte to version enum and hasCloseTx flag.
	var hasCloseTx bool
	s.Version, hasCloseTx = DecodeVersion(version)

	switch s.Version {
	case DefaultSingleVersion:
	case TweaklessCommitVersion:
	case AnchorsCommitVersion:
	case AnchorsZeroFeeHtlcTxCommitVersion:
	case ScriptEnforcedLeaseVersion:
	case SimpleTaprootVersion:
	case TapscriptRootVersion:
	default:
		return fmt.Errorf("unable to de-serialize w/ unknown "+
			"version: %v", s.Version)
	}

	var length uint16
	if err := lnwire.ReadElements(r, &length); err != nil {
		return err
	}

	err = lnwire.ReadElements(
		r, &s.IsInitiator, s.ChainHash[:], &s.FundingOutpoint,
		&s.ShortChannelID, &s.RemoteNodePub, &s.Addresses, &s.Capacity,
	)
	if err != nil {
		return err
	}

	err = lnwire.ReadElements(r, &s.LocalChanCfg.CsvDelay)
	if err != nil {
		return err
	}
	s.LocalChanCfg.MultiSigKey, err = readLocalKeyDesc(r)
	if err != nil {
		return err
	}
	s.LocalChanCfg.RevocationBasePoint, err = readLocalKeyDesc(r)
	if err != nil {
		return err
	}
	s.LocalChanCfg.PaymentBasePoint, err = readLocalKeyDesc(r)
	if err != nil {
		return err
	}
	s.LocalChanCfg.DelayBasePoint, err = readLocalKeyDesc(r)
	if err != nil {
		return err
	}
	s.LocalChanCfg.HtlcBasePoint, err = readLocalKeyDesc(r)
	if err != nil {
		return err
	}

	err = lnwire.ReadElements(r, &s.RemoteChanCfg.CsvDelay)
	if err != nil {
		return err
	}
	s.RemoteChanCfg.MultiSigKey, err = readRemoteKeyDesc(r)
	if err != nil {
		return err
	}
	s.RemoteChanCfg.RevocationBasePoint, err = readRemoteKeyDesc(r)
	if err != nil {
		return err
	}
	s.RemoteChanCfg.PaymentBasePoint, err = readRemoteKeyDesc(r)
	if err != nil {
		return err
	}
	s.RemoteChanCfg.DelayBasePoint, err = readRemoteKeyDesc(r)
	if err != nil {
		return err
	}
	s.RemoteChanCfg.HtlcBasePoint, err = readRemoteKeyDesc(r)
	if err != nil {
		return err
	}

	// Finally, we'll parse out the ShaChainRootDesc.
	var (
		shaChainPub [33]byte
		zeroPub     [33]byte
	)
	if err := lnwire.ReadElements(r, shaChainPub[:]); err != nil {
		return err
	}

	// Since this field is optional, we'll check to see if the pubkey has
	// been specified or not.
	if !bytes.Equal(shaChainPub[:], zeroPub[:]) {
		s.ShaChainRootDesc.PubKey, err = btcec.ParsePubKey(
			shaChainPub[:],
		)
		if err != nil {
			return err
		}
	}

	var shaKeyFam uint32
	if err := lnwire.ReadElements(r, &shaKeyFam); err != nil {
		return err
	}
	s.ShaChainRootDesc.KeyLocator.Family = keychain.KeyFamily(shaKeyFam)
	err = lnwire.ReadElements(r, &s.ShaChainRootDesc.KeyLocator.Index)
	if err != nil {
		return err
	}

	if s.Version == ScriptEnforcedLeaseVersion {
		if err := lnwire.ReadElement(r, &s.LeaseExpiry); err != nil {
			return err
		}
	}

	if !hasCloseTx {
		return nil
	}

	// Deserialize CloseTxInputs if it is present in serialized data.
	commitTx := &wire.MsgTx{}
	if err := commitTx.Deserialize(r); err != nil {
		return err
	}

	var commitSigLen uint16
	if err := lnwire.ReadElement(r, &commitSigLen); err != nil {
		return err
	}
	commitSig := make([]byte, commitSigLen)
	if err := lnwire.ReadElement(r, commitSig); err != nil {
		return err
	}

	var commitHeight uint64
	if s.Version.IsTaproot() {
		err := lnwire.ReadElement(r, &commitHeight)
		if err != nil {
			return err
		}
	}

	tapscriptRootOpt := fn.None[chainhash.Hash]()
	if s.Version.HasTapscriptRoot() {
		var tapscriptRoot chainhash.Hash
		err := lnwire.ReadElement(r, tapscriptRoot[:])
		if err != nil {
			return err
		}
		tapscriptRootOpt = fn.Some(tapscriptRoot)
	}

	s.CloseTxInputs = fn.Some(CloseTxInputs{
		CommitTx:      commitTx,
		CommitSig:     commitSig,
		CommitHeight:  commitHeight,
		TapscriptRoot: tapscriptRootOpt,
	})

	return nil
}

// UnpackFromReader is similar to Deserialize method, but it expects the passed
// io.Reader to contain an encrypt SCB. Refer to the SerializeAndEncrypt method
// for details w.r.t the encryption scheme used. If we're unable to decrypt the
// payload for whatever reason (wrong key, wrong nonce, etc), then this method
// will return an error.
func (s *Single) UnpackFromReader(r io.Reader, keyRing keychain.KeyRing) error {
	e, err := lnencrypt.KeyRingEncrypter(keyRing)
	if err != nil {
		return fmt.Errorf("unable to generate key decrypter %w", err)
	}
	plaintext, err := e.DecryptPayloadFromReader(r)
	if err != nil {
		return err
	}

	// Finally, we'll pack the bytes into a reader to we can deserialize
	// the plaintext bytes of the SCB.
	backupReader := bytes.NewReader(plaintext)
	return s.Deserialize(backupReader)
}

// PackStaticChanBackups accepts a set of existing open channels, and a
// keychain.KeyRing, and returns a map of outpoints to the serialized+encrypted
// static channel backups. The passed keyRing should be backed by the users
// root HD seed in order to ensure full determinism.
func PackStaticChanBackups(backups []Single,
	keyRing keychain.KeyRing) (map[wire.OutPoint][]byte, error) {

	packedBackups := make(map[wire.OutPoint][]byte)
	for _, chanBackup := range backups {
		chanPoint := chanBackup.FundingOutpoint

		var b bytes.Buffer
		err := chanBackup.PackToWriter(&b, keyRing)
		if err != nil {
			return nil, fmt.Errorf("unable to pack chan backup "+
				"for %v: %v", chanPoint, err)
		}

		packedBackups[chanPoint] = b.Bytes()
	}

	return packedBackups, nil
}

// PackedSingles represents a series of fully packed SCBs. This may be the
// combination of a series of individual SCBs in order to batch their
// unpacking.
type PackedSingles [][]byte

// Unpack attempts to decrypt the passed set of encrypted SCBs and deserialize
// each one into a new SCB struct. The passed keyRing should be backed by the
// same HD seed as was used to encrypt the set of backups in the first place.
// If we're unable to decrypt any of the back ups, then we'll return an error.
func (p PackedSingles) Unpack(keyRing keychain.KeyRing) ([]Single, error) {

	backups := make([]Single, len(p))
	for i, encryptedBackup := range p {
		var backup Single

		backupReader := bytes.NewReader(encryptedBackup)
		err := backup.UnpackFromReader(backupReader, keyRing)
		if err != nil {
			return nil, err
		}

		backups[i] = backup
	}

	return backups, nil
}

// TODO(roasbeef): make codec package?
