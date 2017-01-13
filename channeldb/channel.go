package channeldb

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/elkrem"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg/chainhash"
	"github.com/roasbeef/btcd/wire"
	"github.com/roasbeef/btcutil"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Additionally,
	// at the base level of this bucket several prefixed keys are stored which
	// house channel metadata such as total satoshis sent, number of updates
	// etc. These fields are stored at this top level rather than within a
	// node's channel bucket in order to facilitate sequential prefix scans
	// to gather stats such as total satoshis received.
	openChannelBucket = []byte("ocb")

	// chanIDBucket is a third-level bucket stored within a node's ID bucket
	// in the open channel bucket. The resolution path looks something like:
	// ocb -> nodeID -> cib. This bucket contains a series of keys with no
	// values, these keys are the channel ID's of all the active channels
	// we currently have with a specified nodeID. This bucket acts as an
	// additional indexing allowing random access and sequential scans over
	// active channels.
	chanIDBucket = []byte("cib")

	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("ccb")

	// channelLogBucket is dedicated for storing the necessary delta state
	// between channel updates required to re-construct a past state in
	// order to punish a counterparty attempting a non-cooperative channel
	// closure. A channel log bucket is created for each node and is nested
	// within a node's ID bucket.
	channelLogBucket = []byte("clb")

	// identityKey is the key for storing this node's current LD identity
	// key.
	identityKey = []byte("idk")

	// The following prefixes are stored at the base level within the
	// openChannelBucket. In order to retrieve a particular field for an
	// active, or historic channel, append the channels ID to the prefix:
	// key = prefix || chanID. Storing certain fields at the top level
	// using a prefix scheme serves two purposes: first to facilitate
	// sequential prefix scans, and second to eliminate write amplification
	// caused by serializing/deserializing the *entire* struct with each
	// update.
	chanCapacityPrefix   = []byte("ccp")
	selfBalancePrefix    = []byte("sbp")
	theirBalancePrefix   = []byte("tbp")
	minFeePerKbPrefix    = []byte("mfp")
	theirDustLimitPrefix = []byte("tdlp")
	ourDustLimitPrefix   = []byte("odlp")
	updatePrefix         = []byte("uup")
	satSentPrefix        = []byte("ssp")
	satReceivedPrefix    = []byte("srp")
	netFeesPrefix        = []byte("ntp")

	// chanIDKey stores the node, and channelID for an active channel.
	chanIDKey = []byte("cik")

	// commitKeys stores both commitment keys (ours, and theirs) for an
	// active channel. Our private key is stored in an encrypted format
	// using channeldb's currently registered cryptoSystem.
	commitKeys = []byte("ckk")

	// commitTxnsKey stores the full version of both current, non-revoked
	// commitment transactions in addition to the csvDelay for both.
	commitTxnsKey = []byte("ctk")

	// currentHtlcKey stores the set of fully locked-in HTLCs on our
	// latest commitment state.
	currentHtlcKey = []byte("chk")

	// fundingTxnKey stroes the funding tx, our encrypted multi-sig key,
	// and finally 2-of-2 multisig redeem script.
	fundingTxnKey = []byte("fsk")

	// elkremStateKey stores their current revocation hash, and our elkrem
	// sender, and their elkrem receiver.
	elkremStateKey = []byte("esk")

	// deliveryScriptsKey stores the scripts for the final delivery in the
	// case of a cooperative closure.
	deliveryScriptsKey = []byte("dsk")
)

// ChannelType is an enum-like type that describes one of several possible
// channel types. Each open channel is associated with a particular type as the
// channel type may determine how higher level operations are conducted such as
// fee negotiation, channel closing, the format of HTLCs, etc.
// TODO(roasbeef): split up per-chain?
type ChannelType uint8

const (
	// NOTE: iota isn't used here for this enum needs to be stable
	// long-term as it will be persisted to the database.

	// SingleFunder represents a channel wherein one party solely funds the
	// entire capacity of the channel.
	SingleFunder = 0

	// DualFunder represents a channel wherein both parties contribute
	// funds towards the total capacity of the channel. The channel may be
	// funded symmetrically or asymmetrically.
	DualFunder = 1
)

// OpenChannel encapsulates the persistent and dynamic state of an open channel
// with a remote node. An open channel supports several options for on-disk
// serialization depending on the exact context. Full (upon channel creation)
// state commitments, and partial (due to a commitment update) writes are
// supported. Each partial write due to a state update appends the new update
// to an on-disk log, which can then subsequently be queried in order to
// "time-travel" to a prior state.
type OpenChannel struct {
	// IdentityPub is the identity public key of the remote node this
	// channel has been established with.
	IdentityPub *btcec.PublicKey

	// ChanID is an identifier that uniquely identifies this channel
	// globally within the blockchain.
	ChanID *wire.OutPoint

	// MinFeePerKb is the min BTC/KB that should be paid within the
	// commitment transaction for the entire duration of the channel's
	// lifetime. This field may be updated during normal operation of the
	// channel as on-chain conditions change.
	MinFeePerKb btcutil.Amount

	// TheirDustLimit is the threshold below which no HTLC output should be
	// generated for their commitment transaction; ie. HTLCs below
	// this amount are not enforceable onchain from their point of view.
	TheirDustLimit btcutil.Amount

	// OurDustLimit is the threshold below which no HTLC output should be
	// generated for our commitment transaction; ie. HTLCs below
	// this amount are not enforceable onchain from out point of view.
	OurDustLimit btcutil.Amount

	// OurCommitKey is the key to be used within our commitment transaction
	// to generate the scripts for outputs paying to ourself, and
	// revocation clauses.
	OurCommitKey *btcec.PublicKey

	// TheirCommitKey is the key to be used within our commitment
	// transaction to generate the scripts for outputs paying to ourself,
	// and revocation clauses.
	TheirCommitKey *btcec.PublicKey

	// Capacity is the total capacity of this channel.
	// TODO(roasbeef): need another field to mark how much fees have been
	// allocated independent of capacity.
	Capacity btcutil.Amount

	// OurBalance is the current available settled balance within the
	// channel directly spendable by us.
	OurBalance btcutil.Amount

	// TheirBalance is the current available settled balance within the
	// channel directly spendable by the remote node.
	TheirBalance btcutil.Amount

	// OurCommitKey is the latest version of the commitment state,
	// broadcast able by us.
	OurCommitTx *wire.MsgTx

	// OurCommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above.
	OurCommitSig []byte

	// StateHintObsfucator are the btyes selected by the initiator (derived
	// from their shachain root) to obsfucate the state-hint encoded within
	// the commitment transaction.
	StateHintObsfucator [4]byte

	// ChanType denotes which type of channel this is.
	ChanType ChannelType

	// IsInitiator is a bool which indicates if we were the original
	// initiator for the channel. This value may affect how higher levels
	// negotiate fees, or close the channel.
	IsInitiator bool

	// FundingOutpoint is the outpoint of the final funding transaction.
	FundingOutpoint *wire.OutPoint

	// OurMultiSigKey is the multi-sig key used within the funding
	// transaction that we control.
	OurMultiSigKey *btcec.PublicKey

	// TheirMultiSigKey is the multi-sig key used within the funding
	// transaction for the remote party.
	TheirMultiSigKey *btcec.PublicKey

	// FundingWitnessScript is the full witness script used within the
	// funding transaction.
	FundingWitnessScript []byte

	// LocalCsvDelay is the delay to be used in outputs paying to us within
	// the commitment transaction. This value is to be always expressed in
	// terms of relative blocks.
	LocalCsvDelay uint32

	// RemoteCsvDelay is the delay to be used in outputs paying to the
	// remote party. This value is to be always expressed in terms of
	// relative blocks.
	RemoteCsvDelay uint32

	// Current revocation for their commitment transaction. However, since
	// this the derived public key, we don't yet have the preimage so we
	// aren't yet able to verify that it's actually in the hash chain.
	TheirCurrentRevocation     *btcec.PublicKey
	TheirCurrentRevocationHash [32]byte
	LocalElkrem                *elkrem.ElkremSender
	RemoteElkrem               *elkrem.ElkremReceiver

	// OurDeliveryScript is the script to be used to pay to us in
	// cooperative closes.
	OurDeliveryScript []byte

	// OurDeliveryScript is the script to be used to pay to the remote
	// party in cooperative closes.
	TheirDeliveryScript []byte

	// NumUpdates is the total number of updates conducted within this
	// channel.
	NumUpdates uint64

	// TotalSatoshisSent is the total number of satoshis we've sent within
	// this channel.
	TotalSatoshisSent uint64

	// TotalSatoshisReceived is the total number of satoshis we've received
	// within this channel.
	TotalSatoshisReceived uint64

	// CreationTime is the time this channel was initially created.
	CreationTime time.Time

	// Htlcs is the list of active, uncleared HTLCs currently pending
	// within the channel.
	Htlcs []*HTLC

	// TODO(roasbeef): eww
	Db *DB

	sync.RWMutex
}

// FullSync serializes, and writes to disk the *full* channel state, using
// both the active channel bucket to store the prefixed column fields, and the
// remote node's ID to store the remainder of the channel state.
func (c *OpenChannel) FullSync() error {
	c.Lock()
	defer c.Unlock()

	return c.Db.Update(c.fullSync)
}

// fullSync is an internal versino of the FullSync method which allows callers
// to sync the contents of an OpenChannel while re-using an existing database
// transaction.
func (c *OpenChannel) fullSync(tx *bolt.Tx) error {
	// TODO(roasbeef): add helper funcs to create scoped update
	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
	if err != nil {
		return err
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := c.IdentityPub.SerializeCompressed()
	nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(nodePub)
	if err != nil {
		return err
	}

	// Add this channel ID to the node's active channel index if
	// it doesn't already exist.
	chanIDBucket, err := nodeChanBucket.CreateBucketIfNotExists(chanIDBucket)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := writeOutpoint(&b, c.ChanID); err != nil {
		return err
	}
	if chanIDBucket.Get(b.Bytes()) == nil {
		if err := chanIDBucket.Put(b.Bytes(), nil); err != nil {
			return err
		}
	}

	return putOpenChannel(chanBucket, nodeChanBucket, c)
}

// FullSyncWithAddr is identical to the FullSync function in that it writes the
// full channel state to disk. Additionally, this function also creates a
// LinkNode relationship between this newly created channel and an existing of
// new LinkNode instance. Syncing with this method rather than FullSync is
// required in order to allow listing all channels in the database globally, or
// according to the LinkNode they were created with.
//
// TODO(roasbeef): addr param should eventually be a lnwire.NetAddress type
// that includes service bits.
func (c *OpenChannel) FullSyncWithAddr(addr *net.TCPAddr) error {
	c.Lock()
	defer c.Unlock()

	return c.Db.Update(func(tx *bolt.Tx) error {
		// First, sync all the persistent channel state to disk.
		if err := c.fullSync(tx); err != nil {
			return err
		}

		nodeInfoBucket, err := tx.CreateBucketIfNotExists(nodeInfoBucket)
		if err != nil {
			return err
		}

		// If a LinkNode for this identity public key already exsits, then
		// we can exit early.
		nodePub := c.IdentityPub.SerializeCompressed()
		if nodeInfoBucket.Get(nodePub) != nil {
			return nil
		}

		// Next, we need to establish a (possibly) new LinkNode
		// relationship for this channel. The LinkNode metadata contains
		// reachability, up-time, and service bits related information.
		// TODO(roasbeef): net info shuld be in lnwire.NetAddress
		linkNode := c.Db.NewLinkNode(wire.MainNet, c.IdentityPub, addr)

		return putLinkNode(nodeInfoBucket, linkNode)
	})
}

// UpdateCommitment updates the on-disk state of our currently broadcastable
// commitment state. This method is to be called once we have revoked our prior
// commitment state, accepting the new state as defined by the passed
// parameters.
func (c *OpenChannel) UpdateCommitment(newCommitment *wire.MsgTx,
	newSig []byte, delta *ChannelDelta) error {

	c.Lock()
	defer c.Unlock()

	return c.Db.Update(func(tx *bolt.Tx) error {
		chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		id := c.IdentityPub.SerializeCompressed()
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(id)
		if err != nil {
			return err
		}

		// TODO(roasbeef): modify the funcs below to take values
		// directly, otherwise need to roll back to prior state. Could
		// also make copy above, then modify to pass in.
		c.OurCommitTx = newCommitment
		c.OurCommitSig = newSig
		c.OurBalance = delta.LocalBalance
		c.TheirBalance = delta.RemoteBalance
		c.NumUpdates = uint64(delta.UpdateNum)
		c.Htlcs = delta.Htlcs

		// First we'll write out the current latest dynamic channel
		// state: the current channel balance, the number of updates,
		// and our latest commitment transaction+sig.
		// TODO(roasbeef): re-make schema s.t this is a single put
		if err := putChanCapacity(chanBucket, c); err != nil {
			return err
		}
		if err := putChanAmountsTransferred(chanBucket, c); err != nil {
			return err
		}
		if err := putChanNumUpdates(chanBucket, c); err != nil {
			return err
		}
		if err := putChanCommitTxns(nodeChanBucket, c); err != nil {
			return err
		}
		if err := putCurrentHtlcs(nodeChanBucket, delta.Htlcs, c.ChanID); err != nil {
			return err
		}

		return nil
	})
}

// HTLC is the on-disk representation of a hash time-locked contract. HTLCs
// are contained within ChannelDeltas which encode the current state of the
// commitment between state updates.
type HTLC struct {
	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming bool

	// Amt is the amount of satoshis this HTLC escrows.
	Amt btcutil.Amount

	// RHash is the payment hash of the HTLC.
	RHash [32]byte

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout uint32

	// RevocationDelay is the relative timeout the party who broadcasts the
	// commitment transaction must wait before being able to fully sweep
	// the funds on-chain in the case of a unilateral channel closure.
	RevocationDelay uint32

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	OutputIndex uint16
}

// Copy returns a full copy of the target HTLC.
func (h *HTLC) Copy() HTLC {
	clone := HTLC{
		Incoming:        h.Incoming,
		Amt:             h.Amt,
		RefundTimeout:   h.RefundTimeout,
		RevocationDelay: h.RevocationDelay,
		OutputIndex:     h.OutputIndex,
	}
	copy(clone.RHash[:], h.RHash[:])

	return clone
}

// ChannelDelta is a snapshot of the commitment state at a particular point in
// the commitment chain. With each state transition, a snapshot of the current
// state along with all non-settled HTLCs are recorded.
type ChannelDelta struct {
	LocalBalance  btcutil.Amount
	RemoteBalance btcutil.Amount
	UpdateNum     uint32

	// TODO(roasbeef): add blockhash or timestamp?

	Htlcs []*HTLC
}

// AppendToRevocationLog records the new state transition within an on-disk
// append-only log which records all state transitions by the remote peer. In
// the case of an uncooperative broadcast of a prior state by the remote peer,
// this log can be consulted in order to reconstruct the state needed to
// rectify the situation.
func (c *OpenChannel) AppendToRevocationLog(delta *ChannelDelta) error {
	return c.Db.Update(func(tx *bolt.Tx) error {
		chanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		id := c.IdentityPub.SerializeCompressed()
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(id)
		if err != nil {
			return err
		}

		// Persist the latest elkrem state to disk as the remote peer
		// has just added to our local elkrem receiver, and given us a
		// new pending revocation key.
		if err := putChanElkremState(nodeChanBucket, c); err != nil {
			return err
		}

		// With the current elkrem state updated, append a new log
		// entry recording this the delta of this state transition.
		// TODO(roasbeef): could make the deltas relative, would save
		// space, but then tradeoff for more disk-seeks to recover the
		// full state.
		logKey := channelLogBucket
		logBucket, err := nodeChanBucket.CreateBucketIfNotExists(logKey)
		if err != nil {
			return err
		}

		return appendChannelLogEntry(logBucket, delta, c.ChanID)
	})
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to data.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to data.
func (c *OpenChannel) CommitmentHeight() (uint64, error) {
	// TODO(roasbeef): this is super hacky, remedy during refactor!!!
	o := &OpenChannel{
		ChanID: c.ChanID,
	}

	err := c.Db.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		return fetchChanNumUpdates(openChanBucket, o)
	})
	if err != nil {
		return 0, nil
	}

	return o.NumUpdates, nil
}

// FindPreviousState scans through the append-only log in an attempt to recover
// the previous channel state indicated by the update number. This method is
// intended to be used for obtaining the relevant data needed to claim all
// funds rightfully spendable in the case of an on-chain broadcast of the
// commitment transaction.
func (c *OpenChannel) FindPreviousState(updateNum uint64) (*ChannelDelta, error) {
	delta := &ChannelDelta{}

	err := c.Db.View(func(tx *bolt.Tx) error {
		chanBucket := tx.Bucket(openChannelBucket)

		nodePub := c.IdentityPub.SerializeCompressed()
		nodeChanBucket := chanBucket.Bucket(nodePub)
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		logBucket := nodeChanBucket.Bucket(channelLogBucket)
		if nodeChanBucket == nil {
			return ErrNoPastDeltas
		}

		var err error
		delta, err = fetchChannelLogEntry(logBucket, c.ChanID,
			uint32(updateNum))

		return err
	})
	if err != nil {
		return nil, err
	}

	return delta, nil
}

// CloseChannel closes a previously active lightning channel. Closing a channel
// entails deleting all saved state within the database concerning this
// channel, as well as created a small channel summary for record keeping
// purposes.
// TODO(roasbeef): delete on-disk set of HTLCs
func (c *OpenChannel) CloseChannel() error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, c.ChanID); err != nil {
		return err
	}

	return c.Db.Update(func(tx *bolt.Tx) error {
		// First fetch the top level bucket which stores all data related to
		// current, active channels.
		chanBucket := tx.Bucket(openChannelBucket)
		if chanBucket == nil {
			return ErrNoChanDBExists
		}

		// Within this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodePub := c.IdentityPub.SerializeCompressed()
		nodeChanBucket := chanBucket.Bucket(nodePub)
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Delete this channel ID from the node's active channel index.
		chanIndexBucket := nodeChanBucket.Bucket(chanIDBucket)
		if chanIndexBucket == nil {
			return ErrNoActiveChannels
		}

		// If this channel isn't found within the channel index bucket,
		// then it has already been deleted. So we can exit early as
		// there isn't any more work for us to do here.
		outPointBytes := b.Bytes()
		if chanIndexBucket.Get(outPointBytes) == nil {
			return nil
		}

		// Otherwise, we can safely delete the channel from the index
		// without running into any boltdb related errors by repeated
		// deletion attempts.
		if err := chanIndexBucket.Delete(outPointBytes); err != nil {
			return err
		}

		// Now that the index to this channel has been deleted, purge
		// the remaining channel metadata from the database.
		if err := deleteOpenChannel(chanBucket, nodeChanBucket,
			outPointBytes); err != nil {
			return err
		}

		// Finally, create a summary of this channel in the closed
		// channel bucket for this node.
		return putClosedChannelSummary(tx, outPointBytes)
	})
}

// ChannelSnapshot is a frozen snapshot of the current channel state. A
// snapshot is detached from the original channel that generated it, providing
// read-only access to the current or prior state of an active channel.
type ChannelSnapshot struct {
	RemoteIdentity btcec.PublicKey

	ChannelPoint *wire.OutPoint

	Capacity      btcutil.Amount
	LocalBalance  btcutil.Amount
	RemoteBalance btcutil.Amount

	NumUpdates uint64

	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64

	Htlcs []HTLC
}

// Snapshot returns a read-only snapshot of the current channel state. This
// snapshot includes information concerning the current settled balance within
// the channel, metadata detailing total flows, and any outstanding HTLCs.
func (c *OpenChannel) Snapshot() *ChannelSnapshot {
	c.RLock()
	defer c.RUnlock()

	snapshot := &ChannelSnapshot{
		RemoteIdentity:        *c.IdentityPub,
		ChannelPoint:          c.ChanID,
		Capacity:              c.Capacity,
		LocalBalance:          c.OurBalance,
		RemoteBalance:         c.TheirBalance,
		NumUpdates:            c.NumUpdates,
		TotalSatoshisSent:     c.TotalSatoshisSent,
		TotalSatoshisReceived: c.TotalSatoshisReceived,
	}

	// Copy over the current set of HTLCs to ensure the caller can't
	// mutate our internal state.
	snapshot.Htlcs = make([]HTLC, len(c.Htlcs))
	for i, h := range c.Htlcs {
		snapshot.Htlcs[i] = h.Copy()
	}

	return snapshot
}

func putClosedChannelSummary(tx *bolt.Tx, chanID []byte) error {
	// For now, a summary of a closed channel simply involves recording the
	// outpoint of the funding transaction.
	closedChanBucket, err := tx.CreateBucketIfNotExists(closedChannelBucket)
	if err != nil {
		return err
	}

	// TODO(roasbeef): add other info
	//  * should likely have each in own bucket per node
	return closedChanBucket.Put(chanID, nil)
}

// putChannel serializes, and stores the current state of the channel in its
// entirety.
func putOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channel *OpenChannel) error {

	// First write out all the "common" fields using the field's prefix
	// append with the channel's ID. These fields go into a top-level
	// bucket to allow for ease of metric aggregation via efficient prefix
	// scans.
	if err := putChanCapacity(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanMinFeePerKb(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanTheirDustLimit(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanOurDustLimit(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNumUpdates(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanAmountsTransferred(openChanBucket, channel); err != nil {
		return err
	}

	// Next, write out the fields of the channel update less frequently.
	if err := putChannelIDs(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitKeys(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitTxns(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanFundingInfo(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanElkremState(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putCurrentHtlcs(nodeChanBucket, channel.Htlcs,
		channel.ChanID); err != nil {
		return err
	}

	return nil
}

// fetchOpenChannel retrieves, and deserializes (including decrypting
// sensitive) the complete channel currently active with the passed nodeID.
func fetchOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	chanID *wire.OutPoint) (*OpenChannel, error) {

	var err error
	channel := &OpenChannel{
		ChanID: chanID,
	}

	// First, read out the fields of the channel update less frequently.
	if err = fetchChannelIDs(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanCommitKeys(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanCommitTxns(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanFundingInfo(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanElkremState(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	channel.Htlcs, err = fetchCurrentHtlcs(nodeChanBucket, chanID)
	if err != nil {
		return nil, err
	}

	// With the existence of an open channel bucket with this node verified,
	// perform a full read of the entire struct. Starting with the prefixed
	// fields residing in the parent bucket.
	if err = fetchChanCapacity(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanMinFeePerKb(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanTheirDustLimit(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanOurDustLimit(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanNumUpdates(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanAmountsTransferred(openChanBucket, channel); err != nil {
		return nil, err
	}

	return channel, nil
}

func deleteOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channelID []byte) error {

	// First we'll delete all the "common" top level items stored outside
	// the node's channel bucket.
	if err := deleteChanCapacity(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanMinFeePerKb(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanNumUpdates(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanAmountsTransferred(openChanBucket, channelID); err != nil {
		return err
	}

	// Finally, delete all the fields directly within the node's channel
	// bucket.
	if err := deleteChannelIDs(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitKeys(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitTxns(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanFundingInfo(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanElkremState(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanDeliveryScripts(nodeChanBucket, channelID); err != nil {
		return err
	}

	return nil
}

func putChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// Some scratch bytes re-used for serializing each of the uint64's.
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)
	scratch3 := make([]byte, 8)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], chanCapacityPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.Capacity))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], selfBalancePrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.OurBalance))
	if err := openChanBucket.Put(keyPrefix, scratch2); err != nil {
		return err
	}

	copy(keyPrefix[:3], theirBalancePrefix)
	byteOrder.PutUint64(scratch3, uint64(channel.TheirBalance))
	return openChanBucket.Put(keyPrefix, scratch3)
}

func deleteChanCapacity(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix[3:], chanID)

	copy(keyPrefix[:3], chanCapacityPrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], selfBalancePrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], theirBalancePrefix)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// A byte slice re-used to compute each key prefix below.
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], chanCapacityPrefix)
	capacityBytes := openChanBucket.Get(keyPrefix)
	channel.Capacity = btcutil.Amount(byteOrder.Uint64(capacityBytes))

	copy(keyPrefix[:3], selfBalancePrefix)
	selfBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.OurBalance = btcutil.Amount(byteOrder.Uint64(selfBalanceBytes))

	copy(keyPrefix[:3], theirBalancePrefix)
	theirBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.TheirBalance = btcutil.Amount(byteOrder.Uint64(theirBalanceBytes))

	return nil
}

func putChanMinFeePerKb(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.MinFeePerKb))

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func putChanTheirDustLimit(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.TheirDustLimit))

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, theirDustLimitPrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func putChanOurDustLimit(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.OurDustLimit))

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, ourDustLimitPrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanMinFeePerKb(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanMinFeePerKb(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], b.Bytes())

	feeBytes := openChanBucket.Get(keyPrefix)
	channel.MinFeePerKb = btcutil.Amount(byteOrder.Uint64(feeBytes))

	return nil
}

func fetchChanTheirDustLimit(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, theirDustLimitPrefix)
	copy(keyPrefix[3:], b.Bytes())

	dustLimitBytes := openChanBucket.Get(keyPrefix)
	channel.TheirDustLimit = btcutil.Amount(byteOrder.Uint64(dustLimitBytes))

	return nil
}

func fetchChanOurDustLimit(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, ourDustLimitPrefix)
	copy(keyPrefix[3:], b.Bytes())

	dustLimitBytes := openChanBucket.Get(keyPrefix)
	channel.OurDustLimit = btcutil.Amount(byteOrder.Uint64(dustLimitBytes))

	return nil
}

func putChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, channel.NumUpdates)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanNumUpdates(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], b.Bytes())

	updateBytes := openChanBucket.Get(keyPrefix)
	channel.NumUpdates = byteOrder.Uint64(updateBytes)

	return nil
}

func putChanAmountsTransferred(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)

	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.TotalSatoshisSent))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], satReceivedPrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.TotalSatoshisReceived))
	return openChanBucket.Put(keyPrefix, scratch2)
}

func deleteChanAmountsTransferred(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix[3:], chanID)

	copy(keyPrefix[:3], satSentPrefix)
	if err := openChanBucket.Delete(keyPrefix); err != nil {
		return err
	}

	copy(keyPrefix[:3], satReceivedPrefix)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanAmountsTransferred(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	totalSentBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisSent = byteOrder.Uint64(totalSentBytes)

	copy(keyPrefix[:3], satReceivedPrefix)
	totalReceivedBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisReceived = byteOrder.Uint64(totalReceivedBytes)

	return nil
}

func putChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// TODO(roasbeef): just pass in chanID everywhere for puts
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	// Construct the id key: cid || channelID.
	// TODO(roasbeef): abstract out to func
	idKey := make([]byte, len(chanIDKey)+b.Len())
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], b.Bytes())

	idBytes := channel.IdentityPub.SerializeCompressed()
	return nodeChanBucket.Put(idKey, idBytes)
}

func deleteChannelIDs(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	idKey := make([]byte, len(chanIDKey)+len(chanID))
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], chanID)
	return nodeChanBucket.Delete(idKey)
}

func fetchChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var (
		err error
		b   bytes.Buffer
	)

	if err = writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}

	// Construct the id key: cid || channelID.
	idKey := make([]byte, len(chanIDKey)+b.Len())
	copy(idKey[:3], chanIDKey)
	copy(idKey[3:], b.Bytes())

	idBytes := nodeChanBucket.Get(idKey)
	channel.IdentityPub, err = btcec.ParsePubKey(idBytes, btcec.S256())
	if err != nil {
		return err
	}

	return nil
}

func putChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {

	// Construct the key which stores the commitment keys: ckk || channelID.
	// TODO(roasbeef): factor into func
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	commitKey := make([]byte, len(commitKeys)+bc.Len())
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], bc.Bytes())

	var b bytes.Buffer

	if _, err := b.Write(channel.TheirCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	if _, err := b.Write(channel.OurCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	return nodeChanBucket.Put(commitKey, b.Bytes())
}

func deleteChanCommitKeys(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	commitKey := make([]byte, len(commitKeys)+len(chanID))
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], chanID)
	return nodeChanBucket.Delete(commitKey)
}

func fetchChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {

	// Construct the key which stores the commitment keys: ckk || channelID.
	// TODO(roasbeef): factor into func
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	commitKey := make([]byte, len(commitKeys)+bc.Len())
	copy(commitKey[:3], commitKeys)
	copy(commitKey[3:], bc.Bytes())

	var err error
	keyBytes := nodeChanBucket.Get(commitKey)

	channel.TheirCommitKey, err = btcec.ParsePubKey(keyBytes[:33], btcec.S256())
	if err != nil {
		return err
	}

	channel.OurCommitKey, err = btcec.ParsePubKey(keyBytes[33:], btcec.S256())
	if err != nil {
		return err
	}

	return nil
}

func putChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	var b bytes.Buffer

	if err := channel.OurCommitTx.Serialize(&b); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.OurCommitSig); err != nil {
		return err
	}

	// TODO(roasbeef): should move this into putChanFundingInfo
	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, channel.LocalCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}
	byteOrder.PutUint32(scratch, channel.RemoteCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}

	return nodeChanBucket.Put(txnsKey, b.Bytes())
}

func deleteChanCommitTxns(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	txnsKey := make([]byte, len(commitTxnsKey)+len(chanID))
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], chanID)
	return nodeChanBucket.Delete(txnsKey)
}

func fetchChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	var err error
	if err = writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	txnBytes := bytes.NewReader(nodeChanBucket.Get(txnsKey))

	channel.OurCommitTx = wire.NewMsgTx(2)
	if err = channel.OurCommitTx.Deserialize(txnBytes); err != nil {
		return err
	}

	channel.OurCommitSig, err = wire.ReadVarBytes(txnBytes, 0, 80, "")
	if err != nil {
		return err
	}

	scratch := make([]byte, 4)

	if _, err := txnBytes.Read(scratch); err != nil {
		return err
	}
	channel.LocalCsvDelay = byteOrder.Uint32(scratch)

	if _, err := txnBytes.Read(scratch); err != nil {
		return err
	}
	channel.RemoteCsvDelay = byteOrder.Uint32(scratch)

	return nil
}

func putChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+bc.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], bc.Bytes())

	var b bytes.Buffer

	if err := writeOutpoint(&b, channel.FundingOutpoint); err != nil {
		return err
	}

	ourSerKey := channel.OurMultiSigKey.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, ourSerKey); err != nil {
		return err
	}
	theirSerKey := channel.TheirMultiSigKey.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, theirSerKey); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.FundingWitnessScript[:]); err != nil {
		return err
	}

	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.CreationTime.Unix()))
	if _, err := b.Write(scratch); err != nil {
		return err
	}

	var boolByte [1]byte
	if channel.IsInitiator {
		boolByte[0] = 1
	} else {
		boolByte[0] = 0
	}
	if _, err := b.Write(boolByte[:]); err != nil {
		return err
	}

	if _, err := b.Write([]byte{uint8(channel.ChanType)}); err != nil {
		return err
	}

	return nodeChanBucket.Put(fundTxnKey, b.Bytes())
}

func deleteChanFundingInfo(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	fundTxnKey := make([]byte, len(fundingTxnKey)+len(chanID))
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], chanID)
	return nodeChanBucket.Delete(fundTxnKey)
}

func fetchChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+b.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], b.Bytes())

	infoBytes := bytes.NewReader(nodeChanBucket.Get(fundTxnKey))

	// TODO(roasbeef): can remove as channel ID *is* the funding point now.
	channel.FundingOutpoint = &wire.OutPoint{}
	if err := readOutpoint(infoBytes, channel.FundingOutpoint); err != nil {
		return err
	}

	ourKeyBytes, err := wire.ReadVarBytes(infoBytes, 0, 34, "")
	if err != nil {
		return err
	}
	channel.OurMultiSigKey, err = btcec.ParsePubKey(ourKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	theirKeyBytes, err := wire.ReadVarBytes(infoBytes, 0, 34, "")
	if err != nil {
		return err
	}
	channel.TheirMultiSigKey, err = btcec.ParsePubKey(theirKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	channel.FundingWitnessScript, err = wire.ReadVarBytes(infoBytes, 0, 520, "")
	if err != nil {
		return err
	}

	scratch := make([]byte, 8)
	if _, err := infoBytes.Read(scratch); err != nil {
		return err
	}
	unixSecs := byteOrder.Uint64(scratch)
	channel.CreationTime = time.Unix(int64(unixSecs), 0)

	var boolByte [1]byte
	if _, err := io.ReadFull(infoBytes, boolByte[:]); err != nil {
		return err
	}
	if boolByte[0] == 1 {
		channel.IsInitiator = true
	} else {
		channel.IsInitiator = false
	}

	var chanType [1]byte
	if _, err := io.ReadFull(infoBytes, chanType[:]); err != nil {
		return err
	}
	channel.ChanType = ChannelType(chanType[0])

	return nil
}

func putChanElkremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}

	elkremKey := make([]byte, len(elkremStateKey)+bc.Len())
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], bc.Bytes())

	var b bytes.Buffer

	revKey := channel.TheirCurrentRevocation.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, revKey); err != nil {
		return err
	}

	if _, err := b.Write(channel.TheirCurrentRevocationHash[:]); err != nil {
		return err
	}

	// TODO(roasbeef): shouldn't be storing on disk, should re-derive as
	// needed
	senderBytes := channel.LocalElkrem.ToBytes()
	if err := wire.WriteVarBytes(&b, 0, senderBytes); err != nil {
		return err
	}

	reciverBytes, err := channel.RemoteElkrem.ToBytes()
	if err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, reciverBytes); err != nil {
		return err
	}

	if _, err := b.Write(channel.StateHintObsfucator[:]); err != nil {
		return err
	}

	return nodeChanBucket.Put(elkremKey, b.Bytes())
}

func deleteChanElkremState(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	elkremKey := make([]byte, len(elkremStateKey)+len(chanID))
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], chanID)
	return nodeChanBucket.Delete(elkremKey)
}

func fetchChanElkremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	elkremKey := make([]byte, len(elkremStateKey)+b.Len())
	copy(elkremKey[:3], elkremStateKey)
	copy(elkremKey[3:], b.Bytes())

	elkremStateBytes := bytes.NewReader(nodeChanBucket.Get(elkremKey))

	revKeyBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	channel.TheirCurrentRevocation, err = btcec.ParsePubKey(revKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	if _, err := elkremStateBytes.Read(channel.TheirCurrentRevocationHash[:]); err != nil {
		return err
	}

	// TODO(roasbeef): should be rederiving on fly, or encrypting on disk.
	senderBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	elkremRoot, err := chainhash.NewHash(senderBytes)
	if err != nil {
		return err
	}
	channel.LocalElkrem = elkrem.NewElkremSender(*elkremRoot)

	reciverBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	remoteE, err := elkrem.ElkremReceiverFromBytes(reciverBytes)
	if err != nil {
		return err
	}
	channel.RemoteElkrem = remoteE

	_, err = io.ReadFull(elkremStateBytes, channel.StateHintObsfucator[:])
	if err != nil {
		return err
	}

	return nil
}

func putChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, channel.ChanID); err != nil {
		return err
	}
	deliveryKey := make([]byte, len(deliveryScriptsKey)+bc.Len())
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], bc.Bytes())

	var b bytes.Buffer
	if err := wire.WriteVarBytes(&b, 0, channel.OurDeliveryScript); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, channel.TheirDeliveryScript); err != nil {
		return err
	}

	return nodeChanBucket.Put(deliveryScriptsKey, b.Bytes())
}

func deleteChanDeliveryScripts(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	deliveryKey := make([]byte, len(deliveryScriptsKey)+len(chanID))
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], chanID)
	return nodeChanBucket.Delete(deliveryScriptsKey)
}

func fetchChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, channel.ChanID); err != nil {
		return err
	}
	deliveryKey := make([]byte, len(deliveryScriptsKey)+b.Len())
	copy(deliveryKey[:3], deliveryScriptsKey)
	copy(deliveryKey[3:], b.Bytes())

	var err error
	deliveryBytes := bytes.NewReader(nodeChanBucket.Get(deliveryScriptsKey))

	channel.OurDeliveryScript, err = wire.ReadVarBytes(deliveryBytes, 0, 520, "")
	if err != nil {
		return err
	}

	channel.TheirDeliveryScript, err = wire.ReadVarBytes(deliveryBytes, 0, 520, "")
	if err != nil {
		return err
	}

	return nil
}

// htlcDiskSize represents the number of btyes a serialized HTLC takes up on
// disk. The size of an HTLC on disk is 49 bytes total: incoming (1) + amt (8)
// + rhash (32) + timeouts (8) + output index (2)
const htlcDiskSize = 1 + 8 + 32 + 4 + 4 + 2

func serializeHTLC(w io.Writer, h *HTLC) error {
	var buf [htlcDiskSize]byte

	var boolByte [1]byte
	if h.Incoming {
		boolByte[0] = 1
	} else {
		boolByte[0] = 0
	}

	var n int
	n += copy(buf[:], boolByte[:])
	byteOrder.PutUint64(buf[n:], uint64(h.Amt))
	n += 8
	n += copy(buf[n:], h.RHash[:])
	byteOrder.PutUint32(buf[n:], h.RefundTimeout)
	n += 4
	byteOrder.PutUint32(buf[n:], h.RevocationDelay)
	n += 4
	byteOrder.PutUint16(buf[n:], h.OutputIndex)
	n += 2

	if _, err := w.Write(buf[:]); err != nil {
		return err
	}

	return nil
}

func deserializeHTLC(r io.Reader) (*HTLC, error) {
	h := &HTLC{}

	var scratch [8]byte

	if _, err := r.Read(scratch[:1]); err != nil {
		return nil, err
	}
	if scratch[0] == 1 {
		h.Incoming = true
	} else {
		h.Incoming = false
	}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	h.Amt = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(h.RHash[:]); err != nil {
		return nil, err
	}

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	h.RefundTimeout = byteOrder.Uint32(scratch[:4])

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	h.RevocationDelay = byteOrder.Uint32(scratch[:])

	if _, err := io.ReadFull(r, scratch[:2]); err != nil {
		return nil, err
	}
	h.OutputIndex = byteOrder.Uint16(scratch[:])

	return h, nil
}

func makeHtlcKey(o *wire.OutPoint) [39]byte {
	var (
		n int
		k [39]byte
	)

	// chk || txid || index
	n += copy(k[:], currentHtlcKey)
	n += copy(k[n:], o.Hash[:])
	var scratch [4]byte
	byteOrder.PutUint32(scratch[:], o.Index)
	copy(k[n:], scratch[:])

	return k
}

func putCurrentHtlcs(nodeChanBucket *bolt.Bucket, htlcs []*HTLC,
	o *wire.OutPoint) error {
	var b bytes.Buffer

	for _, htlc := range htlcs {
		if err := serializeHTLC(&b, htlc); err != nil {
			return err
		}
	}

	htlcKey := makeHtlcKey(o)
	return nodeChanBucket.Put(htlcKey[:], b.Bytes())
}

func fetchCurrentHtlcs(nodeChanBucket *bolt.Bucket,
	o *wire.OutPoint) ([]*HTLC, error) {

	htlcKey := makeHtlcKey(o)
	htlcBytes := nodeChanBucket.Get(htlcKey[:])
	if htlcBytes == nil {
		return nil, nil
	}

	// TODO(roasbeef): can preallocate here
	var htlcs []*HTLC
	htlcReader := bytes.NewReader(htlcBytes)
	for htlcReader.Len() != 0 {
		htlc, err := deserializeHTLC(htlcReader)
		if err != nil {
			return nil, err
		}

		htlcs = append(htlcs, htlc)
	}

	return htlcs, nil
}

func serializeChannelDelta(w io.Writer, delta *ChannelDelta) error {
	// TODO(roasbeef): could use compression here to reduce on-disk space.
	var scratch [8]byte
	byteOrder.PutUint64(scratch[:], uint64(delta.LocalBalance))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}
	byteOrder.PutUint64(scratch[:], uint64(delta.RemoteBalance))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch[:4], delta.UpdateNum)
	if _, err := w.Write(scratch[:4]); err != nil {
		return err
	}

	numHtlcs := uint64(len(delta.Htlcs))
	if err := wire.WriteVarInt(w, 0, numHtlcs); err != nil {
		return err
	}
	for _, htlc := range delta.Htlcs {
		if err := serializeHTLC(w, htlc); err != nil {
			return err
		}
	}

	return nil
}

func deserializeChannelDelta(r io.Reader) (*ChannelDelta, error) {
	var (
		err     error
		scratch [8]byte
	)

	delta := &ChannelDelta{}

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.LocalBalance = btcutil.Amount(byteOrder.Uint64(scratch[:]))
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.RemoteBalance = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:4]); err != nil {
		return nil, err
	}
	delta.UpdateNum = byteOrder.Uint32(scratch[:4])

	numHtlcs, err := wire.ReadVarInt(r, 0)
	if err != nil {
		return nil, err
	}
	delta.Htlcs = make([]*HTLC, numHtlcs)
	for i := uint64(0); i < numHtlcs; i++ {
		htlc, err := deserializeHTLC(r)
		if err != nil {
			return nil, err
		}

		delta.Htlcs[i] = htlc
	}

	return delta, nil
}

func makeLogKey(o *wire.OutPoint, updateNum uint32) [40]byte {
	var (
		scratch [4]byte
		n       int
		k       [40]byte
	)

	n += copy(k[:], o.Hash[:])

	byteOrder.PutUint32(scratch[:], o.Index)
	copy(k[n:], scratch[:])
	n += 4

	byteOrder.PutUint32(scratch[:], updateNum)
	copy(k[n:], scratch[:])

	return k
}

func appendChannelLogEntry(log *bolt.Bucket, delta *ChannelDelta,
	chanPoint *wire.OutPoint) error {

	var b bytes.Buffer
	if err := serializeChannelDelta(&b, delta); err != nil {
		return err
	}

	logEntrykey := makeLogKey(chanPoint, delta.UpdateNum)
	return log.Put(logEntrykey[:], b.Bytes())
}

func fetchChannelLogEntry(log *bolt.Bucket, chanPoint *wire.OutPoint,
	updateNum uint32) (*ChannelDelta, error) {

	logEntrykey := makeLogKey(chanPoint, updateNum)
	deltaBytes := log.Get(logEntrykey[:])
	if deltaBytes == nil {
		return nil, fmt.Errorf("log entry not found")
	}

	deltaReader := bytes.NewReader(deltaBytes)

	return deserializeChannelDelta(deltaReader)
}

func writeOutpoint(w io.Writer, o *wire.OutPoint) error {
	// TODO(roasbeef): make all scratch buffers on the stack
	scratch := make([]byte, 4)

	// TODO(roasbeef): write raw 32 bytes instead of wasting the extra
	// byte.
	if err := wire.WriteVarBytes(w, 0, o.Hash[:]); err != nil {
		return err
	}

	byteOrder.PutUint32(scratch, o.Index)
	if _, err := w.Write(scratch); err != nil {
		return err
	}

	return nil
}

func readOutpoint(r io.Reader, o *wire.OutPoint) error {
	scratch := make([]byte, 4)

	txid, err := wire.ReadVarBytes(r, 0, 32, "prevout")
	if err != nil {
		return err
	}
	copy(o.Hash[:], txid)

	if _, err := r.Read(scratch); err != nil {
		return err
	}
	o.Index = byteOrder.Uint32(scratch)

	return nil
}
