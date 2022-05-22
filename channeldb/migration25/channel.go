package migration25

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	mig24 "github.com/lightningnetwork/lnd/channeldb/migration24"
	mig "github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// A tlv type definition used to serialize and deserialize a KeyLocator
	// from the database.
	keyLocType tlv.Type = 1
)

var (
	// chanCommitmentKey can be accessed within the sub-bucket for a
	// particular channel. This key stores the up to date commitment state
	// for a particular channel party. Appending a 0 to the end of this key
	// indicates it's the commitment for the local party, and appending a 1
	// to the end of this key indicates it's the commitment for the remote
	// party.
	chanCommitmentKey = []byte("chan-commitment-key")

	// revocationLogBucketLegacy is the legacy bucket where we store the
	// revocation log in old format.
	revocationLogBucketLegacy = []byte("revocation-log-key")

	// localUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the local peer.
	localUpfrontShutdownKey = []byte("local-upfront-shutdown-key")

	// remoteUpfrontShutdownKey can be accessed within the bucket for a
	// channel (identified by its chanPoint). This key stores an optional
	// upfront shutdown script for the remote peer.
	remoteUpfrontShutdownKey = []byte("remote-upfront-shutdown-key")

	// lastWasRevokeKey is a key that stores true when the last update we
	// sent was a revocation and false when it was a commitment signature.
	// This is nil in the case of new channels with no updates exchanged.
	lastWasRevokeKey = []byte("last-was-revoke")

	// ErrNoChanInfoFound is returned when a particular channel does not
	// have any channels state.
	ErrNoChanInfoFound = fmt.Errorf("no chan info found")

	// ErrNoPastDeltas is returned when the channel delta bucket hasn't been
	// created.
	ErrNoPastDeltas = fmt.Errorf("channel has no recorded deltas")

	// ErrLogEntryNotFound is returned when we cannot find a log entry at
	// the height requested in the revocation log.
	ErrLogEntryNotFound = fmt.Errorf("log entry not found")

	// ErrNoCommitmentsFound is returned when a channel has not set
	// commitment states.
	ErrNoCommitmentsFound = fmt.Errorf("no commitments found")
)

// ChannelType is an enum-like type that describes one of several possible
// channel types. Each open channel is associated with a particular type as the
// channel type may determine how higher level operations are conducted such as
// fee negotiation, channel closing, the format of HTLCs, etc. Structure-wise,
// a ChannelType is a bit field, with each bit denoting a modification from the
// base channel type of single funder.
type ChannelType uint8

const (
	// NOTE: iota isn't used here for this enum needs to be stable
	// long-term as it will be persisted to the database.

	// SingleFunderBit represents a channel wherein one party solely funds
	// the entire capacity of the channel.
	SingleFunderBit ChannelType = 0

	// DualFunderBit represents a channel wherein both parties contribute
	// funds towards the total capacity of the channel. The channel may be
	// funded symmetrically or asymmetrically.
	DualFunderBit ChannelType = 1 << 0

	// SingleFunderTweaklessBit is similar to the basic SingleFunder channel
	// type, but it omits the tweak for one's key in the commitment
	// transaction of the remote party.
	SingleFunderTweaklessBit ChannelType = 1 << 1

	// NoFundingTxBit denotes if we have the funding transaction locally on
	// disk. This bit may be on if the funding transaction was crafted by a
	// wallet external to the primary daemon.
	NoFundingTxBit ChannelType = 1 << 2

	// AnchorOutputsBit indicates that the channel makes use of anchor
	// outputs to bump the commitment transaction's effective feerate. This
	// channel type also uses a delayed to_remote output script.
	AnchorOutputsBit ChannelType = 1 << 3

	// FrozenBit indicates that the channel is a frozen channel, meaning
	// that only the responder can decide to cooperatively close the
	// channel.
	FrozenBit ChannelType = 1 << 4

	// ZeroHtlcTxFeeBit indicates that the channel should use zero-fee
	// second-level HTLC transactions.
	ZeroHtlcTxFeeBit ChannelType = 1 << 5

	// LeaseExpirationBit indicates that the channel has been leased for a
	// period of time, constraining every output that pays to the channel
	// initiator with an additional CLTV of the lease maturity.
	LeaseExpirationBit ChannelType = 1 << 6
)

// IsSingleFunder returns true if the channel type if one of the known single
// funder variants.
func (c ChannelType) IsSingleFunder() bool {
	return c&DualFunderBit == 0
}

// IsDualFunder returns true if the ChannelType has the DualFunderBit set.
func (c ChannelType) IsDualFunder() bool {
	return c&DualFunderBit == DualFunderBit
}

// IsTweakless returns true if the target channel uses a commitment that
// doesn't tweak the key for the remote party.
func (c ChannelType) IsTweakless() bool {
	return c&SingleFunderTweaklessBit == SingleFunderTweaklessBit
}

// HasFundingTx returns true if this channel type is one that has a funding
// transaction stored locally.
func (c ChannelType) HasFundingTx() bool {
	return c&NoFundingTxBit == 0
}

// HasAnchors returns true if this channel type has anchor outputs on its
// commitment.
func (c ChannelType) HasAnchors() bool {
	return c&AnchorOutputsBit == AnchorOutputsBit
}

// ZeroHtlcTxFee returns true if this channel type uses second-level HTLC
// transactions signed with zero-fee.
func (c ChannelType) ZeroHtlcTxFee() bool {
	return c&ZeroHtlcTxFeeBit == ZeroHtlcTxFeeBit
}

// IsFrozen returns true if the channel is considered to be "frozen". A frozen
// channel means that only the responder can initiate a cooperative channel
// closure.
func (c ChannelType) IsFrozen() bool {
	return c&FrozenBit == FrozenBit
}

// HasLeaseExpiration returns true if the channel originated from a lease.
func (c ChannelType) HasLeaseExpiration() bool {
	return c&LeaseExpirationBit == LeaseExpirationBit
}

// ChannelStatus is a bit vector used to indicate whether an OpenChannel is in
// the default usable state, or a state where it shouldn't be used.
type ChannelStatus uint8

var (
	// ChanStatusDefault is the normal state of an open channel.
	ChanStatusDefault ChannelStatus

	// ChanStatusBorked indicates that the channel has entered an
	// irreconcilable state, triggered by a state desynchronization or
	// channel breach.  Channels in this state should never be added to the
	// htlc switch.
	ChanStatusBorked ChannelStatus = 1

	// ChanStatusCommitBroadcasted indicates that a commitment for this
	// channel has been broadcasted.
	ChanStatusCommitBroadcasted ChannelStatus = 1 << 1

	// ChanStatusLocalDataLoss indicates that we have lost channel state
	// for this channel, and broadcasting our latest commitment might be
	// considered a breach.
	//
	// TODO(halseh): actually enforce that we are not force closing such a
	// channel.
	ChanStatusLocalDataLoss ChannelStatus = 1 << 2

	// ChanStatusRestored is a status flag that signals that the channel
	// has been restored, and doesn't have all the fields a typical channel
	// will have.
	ChanStatusRestored ChannelStatus = 1 << 3

	// ChanStatusCoopBroadcasted indicates that a cooperative close for
	// this channel has been broadcasted. Older cooperatively closed
	// channels will only have this status set. Newer ones will also have
	// close initiator information stored using the local/remote initiator
	// status. This status is set in conjunction with the initiator status
	// so that we do not need to check multiple channel statues for
	// cooperative closes.
	ChanStatusCoopBroadcasted ChannelStatus = 1 << 4

	// ChanStatusLocalCloseInitiator indicates that we initiated closing
	// the channel.
	ChanStatusLocalCloseInitiator ChannelStatus = 1 << 5

	// ChanStatusRemoteCloseInitiator indicates that the remote node
	// initiated closing the channel.
	ChanStatusRemoteCloseInitiator ChannelStatus = 1 << 6
)

// chanStatusStrings maps a ChannelStatus to a human friendly string that
// describes that status.
var chanStatusStrings = map[ChannelStatus]string{
	ChanStatusDefault:              "ChanStatusDefault",
	ChanStatusBorked:               "ChanStatusBorked",
	ChanStatusCommitBroadcasted:    "ChanStatusCommitBroadcasted",
	ChanStatusLocalDataLoss:        "ChanStatusLocalDataLoss",
	ChanStatusRestored:             "ChanStatusRestored",
	ChanStatusCoopBroadcasted:      "ChanStatusCoopBroadcasted",
	ChanStatusLocalCloseInitiator:  "ChanStatusLocalCloseInitiator",
	ChanStatusRemoteCloseInitiator: "ChanStatusRemoteCloseInitiator",
}

// orderedChanStatusFlags is an in-order list of all that channel status flags.
var orderedChanStatusFlags = []ChannelStatus{
	ChanStatusBorked,
	ChanStatusCommitBroadcasted,
	ChanStatusLocalDataLoss,
	ChanStatusRestored,
	ChanStatusCoopBroadcasted,
	ChanStatusLocalCloseInitiator,
	ChanStatusRemoteCloseInitiator,
}

// String returns a human-readable representation of the ChannelStatus.
func (c ChannelStatus) String() string {
	// If no flags are set, then this is the default case.
	if c == ChanStatusDefault {
		return chanStatusStrings[ChanStatusDefault]
	}

	// Add individual bit flags.
	statusStr := ""
	for _, flag := range orderedChanStatusFlags {
		if c&flag == flag {
			statusStr += chanStatusStrings[flag] + "|"
			c -= flag
		}
	}

	// Remove anything to the right of the final bar, including it as well.
	statusStr = strings.TrimRight(statusStr, "|")

	// Add any remaining flags which aren't accounted for as hex.
	if c != 0 {
		statusStr += "|0x" + strconv.FormatUint(uint64(c), 16)
	}

	// If this was purely an unknown flag, then remove the extra bar at the
	// start of the string.
	statusStr = strings.TrimLeft(statusStr, "|")

	return statusStr
}

// OpenChannel embeds a mig.OpenChannel with the extra update-to-date fields.
//
// NOTE: doesn't have the Packager field as it's not used in current migration.
type OpenChannel struct {
	mig.OpenChannel

	// ChanType denotes which type of channel this is.
	ChanType ChannelType

	// ChanStatus is the current status of this channel. If it is not in
	// the state Default, it should not be used for forwarding payments.
	//
	// NOTE: In `channeldb.OpenChannel`, this field is private. We choose
	// to export this private field such that following migrations can
	// access this field directly.
	ChanStatus ChannelStatus

	// InitialLocalBalance is the balance we have during the channel
	// opening. When we are not the initiator, this value represents the
	// push amount.
	InitialLocalBalance lnwire.MilliSatoshi

	// InitialRemoteBalance is the balance they have during the channel
	// opening.
	InitialRemoteBalance lnwire.MilliSatoshi

	// LocalShutdownScript is set to a pre-set script if the channel was
	// opened by the local node with option_upfront_shutdown_script set. If
	// the option was not set, the field is empty.
	LocalShutdownScript lnwire.DeliveryAddress

	// RemoteShutdownScript is set to a pre-set script if the channel was
	// opened by the remote node with option_upfront_shutdown_script set.
	// If the option was not set, the field is empty.
	RemoteShutdownScript lnwire.DeliveryAddress

	// ThawHeight is the height when a frozen channel once again becomes a
	// normal channel. If this is zero, then there're no restrictions on
	// this channel. If the value is lower than 500,000, then it's
	// interpreted as a relative height, or an absolute height otherwise.
	ThawHeight uint32

	// LastWasRevoke is a boolean that determines if the last update we
	// sent was a revocation (true) or a commitment signature (false).
	LastWasRevoke bool

	// RevocationKeyLocator stores the KeyLocator information that we will
	// need to derive the shachain root for this channel. This allows us to
	// have private key isolation from lnd.
	RevocationKeyLocator keychain.KeyLocator
}

func (c *OpenChannel) hasChanStatus(status ChannelStatus) bool {
	// Special case ChanStatusDefualt since it isn't actually flag, but a
	// particular combination (or lack-there-of) of flags.
	if status == ChanStatusDefault {
		return c.ChanStatus == ChanStatusDefault
	}

	return c.ChanStatus&status == status
}

// FundingTxPresent returns true if expect the funding transcation to be found
// on disk or already populated within the passed open channel struct.
func (c *OpenChannel) FundingTxPresent() bool {
	chanType := c.ChanType

	return chanType.IsSingleFunder() && chanType.HasFundingTx() &&
		c.IsInitiator &&
		!c.hasChanStatus(ChanStatusRestored)
}

// fetchChanInfo deserializes the channel info based on the legacy boolean.
func fetchChanInfo(chanBucket kvdb.RBucket, c *OpenChannel, legacy bool) error {
	infoBytes := chanBucket.Get(chanInfoKey)
	if infoBytes == nil {
		return ErrNoChanInfoFound
	}
	r := bytes.NewReader(infoBytes)

	var (
		chanType   mig.ChannelType
		chanStatus mig.ChannelStatus
	)

	if err := mig.ReadElements(r,
		&chanType, &c.ChainHash, &c.FundingOutpoint,
		&c.ShortChannelID, &c.IsPending, &c.IsInitiator,
		&chanStatus, &c.FundingBroadcastHeight,
		&c.NumConfsRequired, &c.ChannelFlags,
		&c.IdentityPub, &c.Capacity, &c.TotalMSatSent,
		&c.TotalMSatReceived,
	); err != nil {
		return err
	}

	c.ChanType = ChannelType(chanType)
	c.ChanStatus = ChannelStatus(chanStatus)

	// If this is not the legacy format, we need to read the extra two new
	// fields.
	if !legacy {
		if err := mig.ReadElements(r,
			&c.InitialLocalBalance, &c.InitialRemoteBalance,
		); err != nil {
			return err
		}
	}

	// For single funder channels that we initiated and have the funding
	// transaction to, read the funding txn.
	if c.FundingTxPresent() {
		if err := mig.ReadElement(r, &c.FundingTxn); err != nil {
			return err
		}
	}

	if err := mig.ReadChanConfig(r, &c.LocalChanCfg); err != nil {
		return err
	}
	if err := mig.ReadChanConfig(r, &c.RemoteChanCfg); err != nil {
		return err
	}

	// Retrieve the boolean stored under lastWasRevokeKey.
	lastWasRevokeBytes := chanBucket.Get(lastWasRevokeKey)
	if lastWasRevokeBytes == nil {
		// If nothing has been stored under this key, we store false in
		// the OpenChannel struct.
		c.LastWasRevoke = false
	} else {
		// Otherwise, read the value into the LastWasRevoke field.
		revokeReader := bytes.NewReader(lastWasRevokeBytes)
		err := mig.ReadElements(revokeReader, &c.LastWasRevoke)
		if err != nil {
			return err
		}
	}

	keyLocRecord := MakeKeyLocRecord(keyLocType, &c.RevocationKeyLocator)
	tlvStream, err := tlv.NewStream(keyLocRecord)
	if err != nil {
		return err
	}

	if err := tlvStream.Decode(r); err != nil {
		return err
	}

	// Finally, read the optional shutdown scripts.
	if err := GetOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, &c.LocalShutdownScript,
	); err != nil {
		return err
	}

	return GetOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, &c.RemoteShutdownScript,
	)
}

// fetchChanInfo serializes the channel info based on the legacy boolean and
// saves it to disk.
func putChanInfo(chanBucket kvdb.RwBucket, c *OpenChannel, legacy bool) error {
	var w bytes.Buffer
	if err := mig.WriteElements(&w,
		mig.ChannelType(c.ChanType), c.ChainHash, c.FundingOutpoint,
		c.ShortChannelID, c.IsPending, c.IsInitiator,
		mig.ChannelStatus(c.ChanStatus), c.FundingBroadcastHeight,
		c.NumConfsRequired, c.ChannelFlags,
		c.IdentityPub, c.Capacity, c.TotalMSatSent,
		c.TotalMSatReceived,
	); err != nil {
		return err
	}

	// If this is not legacy format, we need to write the extra two fields.
	if !legacy {
		if err := mig.WriteElements(&w,
			c.InitialLocalBalance, c.InitialRemoteBalance,
		); err != nil {
			return err
		}
	}

	// For single funder channels that we initiated, and we have the
	// funding transaction, then write the funding txn.
	if c.FundingTxPresent() {
		if err := mig.WriteElement(&w, c.FundingTxn); err != nil {
			return err
		}
	}

	if err := mig.WriteChanConfig(&w, &c.LocalChanCfg); err != nil {
		return err
	}
	if err := mig.WriteChanConfig(&w, &c.RemoteChanCfg); err != nil {
		return err
	}

	// Write the RevocationKeyLocator as the first entry in a tlv stream.
	keyLocRecord := MakeKeyLocRecord(
		keyLocType, &c.RevocationKeyLocator,
	)

	tlvStream, err := tlv.NewStream(keyLocRecord)
	if err != nil {
		return err
	}

	if err := tlvStream.Encode(&w); err != nil {
		return err
	}

	if err := chanBucket.Put(chanInfoKey, w.Bytes()); err != nil {
		return err
	}

	// Finally, add optional shutdown scripts for the local and remote peer
	// if they are present.
	if err := PutOptionalUpfrontShutdownScript(
		chanBucket, localUpfrontShutdownKey, c.LocalShutdownScript,
	); err != nil {
		return err
	}

	return PutOptionalUpfrontShutdownScript(
		chanBucket, remoteUpfrontShutdownKey, c.RemoteShutdownScript,
	)
}

// EKeyLocator is an encoder for keychain.KeyLocator.
func EKeyLocator(w io.Writer, val interface{}, buf *[8]byte) error {
	if v, ok := val.(*keychain.KeyLocator); ok {
		err := tlv.EUint32T(w, uint32(v.Family), buf)
		if err != nil {
			return err
		}

		return tlv.EUint32T(w, v.Index, buf)
	}
	return tlv.NewTypeForEncodingErr(val, "keychain.KeyLocator")
}

// DKeyLocator is a decoder for keychain.KeyLocator.
func DKeyLocator(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if v, ok := val.(*keychain.KeyLocator); ok {
		var family uint32
		err := tlv.DUint32(r, &family, buf, 4)
		if err != nil {
			return err
		}
		v.Family = keychain.KeyFamily(family)

		return tlv.DUint32(r, &v.Index, buf, 4)
	}
	return tlv.NewTypeForDecodingErr(val, "keychain.KeyLocator", l, 8)
}

// MakeKeyLocRecord creates a Record out of a KeyLocator using the passed
// Type and the EKeyLocator and DKeyLocator functions. The size will always be
// 8 as KeyFamily is uint32 and the Index is uint32.
func MakeKeyLocRecord(typ tlv.Type, keyLoc *keychain.KeyLocator) tlv.Record {
	return tlv.MakeStaticRecord(typ, keyLoc, 8, EKeyLocator, DKeyLocator)
}

// PutOptionalUpfrontShutdownScript adds a shutdown script under the key
// provided if it has a non-zero length.
func PutOptionalUpfrontShutdownScript(chanBucket kvdb.RwBucket, key []byte,
	script []byte) error {
	// If the script is empty, we do not need to add anything.
	if len(script) == 0 {
		return nil
	}

	var w bytes.Buffer
	if err := mig.WriteElement(&w, script); err != nil {
		return err
	}

	return chanBucket.Put(key, w.Bytes())
}

// GetOptionalUpfrontShutdownScript reads the shutdown script stored under the
// key provided if it is present. Upfront shutdown scripts are optional, so the
// function returns with no error if the key is not present.
func GetOptionalUpfrontShutdownScript(chanBucket kvdb.RBucket, key []byte,
	script *lnwire.DeliveryAddress) error {

	// Return early if the bucket does not exit, a shutdown script was not
	// set.
	bs := chanBucket.Get(key)
	if bs == nil {
		return nil
	}

	var tempScript []byte
	r := bytes.NewReader(bs)
	if err := mig.ReadElement(r, &tempScript); err != nil {
		return err
	}
	*script = tempScript

	return nil
}

// FetchChanCommitments fetches both the local and remote commitments. This
// function is exported so it can be used by later migrations.
func FetchChanCommitments(chanBucket kvdb.RBucket, channel *OpenChannel) error {
	var err error

	// If this is a restored channel, then we don't have any commitments to
	// read.
	if channel.hasChanStatus(ChanStatusRestored) {
		return nil
	}

	channel.LocalCommitment, err = FetchChanCommitment(chanBucket, true)
	if err != nil {
		return err
	}
	channel.RemoteCommitment, err = FetchChanCommitment(chanBucket, false)
	if err != nil {
		return err
	}

	return nil
}

// FetchChanCommitment fetches a channel commitment. This function is exported
// so it can be used by later migrations.
func FetchChanCommitment(chanBucket kvdb.RBucket,
	local bool) (mig.ChannelCommitment, error) {

	commitKey := chanCommitmentKey
	if local {
		commitKey = append(commitKey, byte(0x00))
	} else {
		commitKey = append(commitKey, byte(0x01))
	}

	commitBytes := chanBucket.Get(commitKey)
	if commitBytes == nil {
		return mig.ChannelCommitment{}, ErrNoCommitmentsFound
	}

	r := bytes.NewReader(commitBytes)
	return mig.DeserializeChanCommit(r)
}

func PutChanCommitment(chanBucket kvdb.RwBucket, c *mig.ChannelCommitment,
	local bool) error {

	commitKey := chanCommitmentKey
	if local {
		commitKey = append(commitKey, byte(0x00))
	} else {
		commitKey = append(commitKey, byte(0x01))
	}

	var b bytes.Buffer
	if err := mig.SerializeChanCommit(&b, c); err != nil {
		return err
	}

	return chanBucket.Put(commitKey, b.Bytes())
}

func PutChanCommitments(chanBucket kvdb.RwBucket, channel *OpenChannel) error {
	// If this is a restored channel, then we don't have any commitments to
	// write.
	if channel.hasChanStatus(ChanStatusRestored) {
		return nil
	}

	err := PutChanCommitment(
		chanBucket, &channel.LocalCommitment, true,
	)
	if err != nil {
		return err
	}

	return PutChanCommitment(
		chanBucket, &channel.RemoteCommitment, false,
	)
}

// balancesAtHeight returns the local and remote balances on our commitment
// transactions as of a given height. This function is not exported as it's
// deprecated.
//
// NOTE: these are our balances *after* subtracting the commitment fee and
// anchor outputs.
func (c *OpenChannel) balancesAtHeight(chanBucket kvdb.RBucket,
	height uint64) (lnwire.MilliSatoshi, lnwire.MilliSatoshi, error) {

	// If our current commit is as the desired height, we can return our
	// current balances.
	if c.LocalCommitment.CommitHeight == height {
		return c.LocalCommitment.LocalBalance,
			c.LocalCommitment.RemoteBalance, nil
	}

	// If our current remote commit is at the desired height, we can return
	// the current balances.
	if c.RemoteCommitment.CommitHeight == height {
		return c.RemoteCommitment.LocalBalance,
			c.RemoteCommitment.RemoteBalance, nil
	}

	// If we are not currently on the height requested, we need to look up
	// the previous height to obtain our balances at the given height.
	commit, err := c.FindPreviousStateLegacy(chanBucket, height)
	if err != nil {
		return 0, 0, err
	}

	return commit.LocalBalance, commit.RemoteBalance, nil
}

// FindPreviousStateLegacy scans through the append-only log in an attempt to
// recover the previous channel state indicated by the update number. This
// method is intended to be used for obtaining the relevant data needed to
// claim all funds rightfully spendable in the case of an on-chain broadcast of
// the commitment transaction.
func (c *OpenChannel) FindPreviousStateLegacy(chanBucket kvdb.RBucket,
	updateNum uint64) (*mig.ChannelCommitment, error) {

	c.RLock()
	defer c.RUnlock()

	logBucket := chanBucket.NestedReadBucket(revocationLogBucketLegacy)
	if logBucket == nil {
		return nil, ErrNoPastDeltas
	}

	commit, err := fetchChannelLogEntry(logBucket, updateNum)
	if err != nil {
		return nil, err
	}

	return &commit, nil
}

func fetchChannelLogEntry(log kvdb.RBucket,
	updateNum uint64) (mig.ChannelCommitment, error) {

	logEntrykey := mig24.MakeLogKey(updateNum)
	commitBytes := log.Get(logEntrykey[:])
	if commitBytes == nil {
		return mig.ChannelCommitment{}, ErrLogEntryNotFound
	}

	commitReader := bytes.NewReader(commitBytes)
	return mig.DeserializeChanCommit(commitReader)
}

func CreateChanBucket(tx kvdb.RwTx, c *OpenChannel) (kvdb.RwBucket, error) {
	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket, err := tx.CreateTopLevelBucket(openChannelBucket)
	if err != nil {
		return nil, err
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := c.IdentityPub.SerializeCompressed()
	nodeChanBucket, err := openChanBucket.CreateBucketIfNotExists(nodePub)
	if err != nil {
		return nil, err
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket, err := nodeChanBucket.CreateBucketIfNotExists(
		c.ChainHash[:],
	)
	if err != nil {
		return nil, err
	}

	var chanPointBuf bytes.Buffer
	err = mig.WriteOutpoint(&chanPointBuf, &c.FundingOutpoint)
	if err != nil {
		return nil, err
	}

	// With the bucket for the node fetched, we can now go down another
	// level, creating the bucket for this channel itself.
	return chainBucket.CreateBucketIfNotExists(chanPointBuf.Bytes())
}
