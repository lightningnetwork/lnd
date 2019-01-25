package chanbackup

import (
	"bytes"
	"fmt"
	"io"
	"net"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SingleBackupVersion denotes the version of the single static channel backup.
// Based on this version, we know how to pack/unpack serialized versions of the
// backup.
type SingleBackupVersion byte

const (
	// DefaultSingleVersion is the defautl version of the single channel
	// backup. The seralized version of this static channel backup is
	// simply: version || SCB. Where SCB is the known format of the
	// version.
	DefaultSingleVersion = 0
)

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
	ShortChannelID lnwire.ShortChannelID

	// RemoteNodePub is the identity public key of the remote node this
	// channel has been established with.
	RemoteNodePub *btcec.PublicKey

	// Addresses is a list of IP address in which either we were able to
	// reach the node over in the past, OR we received an incoming
	// authenticated connection for the stored identity public key.
	Addresses []net.Addr

	// CsvDelay is the local CSV delay used within the channel. We may need
	// this value to reconstruct our script to recover the funds on-chain
	// after a force close.
	CsvDelay uint16

	// PaymentBasePoint describes how to derive base public that's used to
	// deriving the key used within the non-delayed pay-to-self output on
	// the commitment transaction for a node. With this information, we can
	// re-derive the private key needed to sweep the funds on-chain.
	PaymentBasePoint keychain.KeyLocator

	// ShaChainRootDesc describes how to derive the private key that was
	// used as the shachain root for this channel.
	ShaChainRootDesc keychain.KeyDescriptor
}

// NewSingle creates a new static channel backup based on an existing open
// channel. We also pass in the set of addresses that we used in the past to
// connect to the channel peer.
func NewSingle(channel *channeldb.OpenChannel,
	nodeAddrs []net.Addr) Single {

	chanCfg := channel.LocalChanCfg

	// TODO(roasbeef): update after we start to store the KeyLoc for
	// shachain root

	// We'll need to obtain the shachain root which is derived directly
	// from a private key in our keychain.
	var b bytes.Buffer
	channel.RevocationProducer.Encode(&b) // Can't return an error.

	// Once we have the root, we'll make a public key from it, such that
	// the backups plaintext don't carry any private information. When we
	// go to recover, we'll present this in order to derive the private
	// key.
	_, shaChainPoint := btcec.PrivKeyFromBytes(btcec.S256(), b.Bytes())

	return Single{
		ChainHash:        channel.ChainHash,
		FundingOutpoint:  channel.FundingOutpoint,
		ShortChannelID:   channel.ShortChannelID,
		RemoteNodePub:    channel.IdentityPub,
		Addresses:        nodeAddrs,
		CsvDelay:         chanCfg.CsvDelay,
		PaymentBasePoint: chanCfg.PaymentBasePoint.KeyLocator,
		ShaChainRootDesc: keychain.KeyDescriptor{
			PubKey: shaChainPoint,
			KeyLocator: keychain.KeyLocator{
				Family: keychain.KeyFamilyRevocationRoot,
			},
		},
	}
}

// Serialize attempts to write out the serialized version of the target
// StaticChannelBackup into the passed io.Writer.
func (s *Single) Serialize(w io.Writer) error {
	// Check to ensure that we'll only attempt to serialize a version that
	// we're aware of.
	switch s.Version {
	case DefaultSingleVersion:
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
		s.ChainHash[:],
		s.FundingOutpoint,
		s.ShortChannelID,
		s.RemoteNodePub,
		s.Addresses,
		s.CsvDelay,
		uint32(s.PaymentBasePoint.Family),
		s.PaymentBasePoint.Index,
		shaChainPub[:],
		uint32(s.ShaChainRootDesc.KeyLocator.Family),
		s.ShaChainRootDesc.KeyLocator.Index,
	); err != nil {
		return err
	}

	return lnwire.WriteElements(
		w,
		byte(s.Version),
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
// the golden keychain.KeyFamilyStaticBackup base encryption key.  We then take
// the serialized resulting shared secret point, and hash it using sha256 to
// obtain the key that we'll use for encryption. When using the AEAD, we pass
// the nonce as associated data such that we'll be able to package the two
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
	return encryptPayloadToWriter(rawBytes, w, keyRing)
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

	s.Version = SingleBackupVersion(version)

	switch s.Version {
	case DefaultSingleVersion:
	default:
		return fmt.Errorf("unable to de-serialize w/ unknown "+
			"version: %v", s.Version)
	}

	var length uint16
	if err := lnwire.ReadElements(r, &length); err != nil {
		return err
	}

	err = lnwire.ReadElements(
		r, s.ChainHash[:], &s.FundingOutpoint, &s.ShortChannelID,
		&s.RemoteNodePub, &s.Addresses, &s.CsvDelay,
	)
	if err != nil {
		return err
	}

	var keyFam uint32
	if err := lnwire.ReadElements(r, &keyFam); err != nil {
		return err
	}
	s.PaymentBasePoint.Family = keychain.KeyFamily(keyFam)

	err = lnwire.ReadElements(r, &s.PaymentBasePoint.Index)
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
	// ben specified or not.
	if !bytes.Equal(shaChainPub[:], zeroPub[:]) {
		s.ShaChainRootDesc.PubKey, err = btcec.ParsePubKey(
			shaChainPub[:], btcec.S256(),
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

	return lnwire.ReadElements(r, &s.ShaChainRootDesc.KeyLocator.Index)
}

// UnpackFromReader is similar to Deserialize method, but it expects the passed
// io.Reader to contain an encrypt SCB. Refer to the SerializeAndEncrypt method
// for details w.r.t the encryption scheme used. If we're unable to decrypt the
// payload for whatever reason (wrong key, wrong nonce, etc), then this method
// will return an error.
func (s *Single) UnpackFromReader(r io.Reader, keyRing keychain.KeyRing) error {
	plaintext, err := decryptPayloadFromReader(r, keyRing)
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
