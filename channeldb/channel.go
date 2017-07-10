package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/boltdb/bolt"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/shachain"
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
	chanCapacityPrefix = []byte("ccp")
	selfBalancePrefix  = []byte("sbp")
	theirBalancePrefix = []byte("tbp")
	minFeePerKwPrefix  = []byte("mfp")
	chanConfigPrefix   = []byte("chan-config")
	updatePrefix       = []byte("uup")
	satSentPrefix      = []byte("ssp")
	satReceivedPrefix  = []byte("srp")
	commitFeePrefix    = []byte("cfp")
	isPendingPrefix    = []byte("pdg")
	confInfoPrefix     = []byte("conf-info")

	// chanIDKey stores the node, and channelID for an active channel.
	chanIDKey = []byte("cik")

	// commitKeys stores both commitment keys (ours, and theirs) for an
	// active channel. Our private key is stored in an encrypted format
	// using channeldb's currently registered cryptoSystem.
	commitKeys = []byte("ckk")

	// commitTxnsKey stores the full version of both current, non-revoked
	// commitment transactions in addition to the csvDelay for both.
	commitTxnsKey = []byte("ctk")

	// currentHtlcKey stores the set of fully locked-in HTLCs on our latest
	// commitment state.
	currentHtlcKey = []byte("chk")

	// fundingTxnKey stores the funding output, the multi-sig keys used in
	// the funding output, and further information detailing if the
	// transaction is "open", or not and how many confirmations required
	// until it's considered open.
	fundingTxnKey = []byte("fsk")

	// revocationStateKey stores their current revocation hash, our
	// preimage producer and their preimage store.
	revocationStateKey = []byte("esk")
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

// ChannelConstraints represents a set of constraints meant to allow a node to
// limit their exposure, enact flow control and ensure that all HTLC's are
// economically relevant This struct will be mirrored for both sides of the
// channel, as each side will enforce various constraints that MUST be adhered
// to for the life time of the channel. The parameters for each of these
// constraints is static for the duration of the channel, meaning the channel
// must be teared down for them to change.
type ChannelConstraints struct {
	// DustLimit is the threhsold (in satoshis) below which any outputs
	// should be trimmed. When an output is trimmed, it isn't materialized
	// as an actual output, but is instead burned to miner's fees.
	DustLimit btcutil.Amount

	// MaxPendingAmount is the maximum pending HTLC value that can be
	// present within the channel at a particular time. This value is set
	// by the initiator of the channel and must be upheld at all times.
	MaxPendingAmount lnwire.MilliSatoshi

	// ChanReserve is an absolute reservation on the channel for this
	// particular node. This means that the current settled balance for
	// this node CANNOT dip below the reservation amount. This acts as a
	// defense against costless attacks when either side no longer has any
	// skin in the game.
	//
	// TODO(roasbeef): need to swap above, i tell them what reserve, then
	// other way around
	ChanReserve btcutil.Amount

	// MinHTLC is the minimum HTLC accepted for a direction of the channel.
	// If any HTLC's below this amount are offered, then the HTLC will be
	// rejected. This, in tandem with the dust limit allows a node to
	// regulate the smallest HTLC that it deems economically relevant.
	MinHTLC lnwire.MilliSatoshi

	// MaxAcceptedHtlcs is the maximum amount of HTLC's that are to be
	// accepted by the owner of this set of constraints. This allows each
	// node to limit their over all exposure to HTLC's that may need to be
	// acted upon in the case of a unilateral channel closure or a contract
	// breach.
	MaxAcceptedHtlcs uint16
}

// ChannelConfig is a struct that houses the various configuration opens for
// channels. Each side maintains an instance of this configuration file as it
// governs: how the funding and commitment transaction to be created, the
// nature of HTLC's allotted, the keys to be used for delivery, and relative
// time lock parameters.
type ChannelConfig struct {
	// ChannelConstraints is the set of constraints that must be upheld for
	// the duration of the channel for ths owner of this channel
	// configuration. Constraints govern a number of flow control related
	// parameters, also including the smallest HTLC that will be accepted
	// by a participant.
	ChannelConstraints

	// CsvDelay is the relative time lock delay expressed in blocks. Any
	// settled outputs that pay to the owner of this channel configuration
	// MUST ensure that the delay branch uses this value as the relative
	// time lock. Similarly, any HTLC's offered by this node should use
	// this value as well.
	CsvDelay uint16

	// MultiSigKey is the key to be used within the 2-of-2 output script
	// for the owner of this channel config.
	MultiSigKey *btcec.PublicKey

	// RevocationBasePoint is the base public key to be used when deriving
	// revocation keys for the remote node's commitment transaction. This
	// will be combined along with a per commitment secret to derive a
	// unique revocation key for each state.
	RevocationBasePoint *btcec.PublicKey

	// PaymentBasePoint is the based public key to be used when deriving
	// the key used within the non-delayed pay-to-self output on the
	// commitment transaction for a node. This will be combined with a
	// tweak derived from the per-commitment point to ensure unique keys
	// for each commitment transaction.
	PaymentBasePoint *btcec.PublicKey

	// DelayBasePoint is the based public key to be used when deriving the
	// key used within the delayed pay-to-self output on the commitment
	// transaction for a node. This will be combined with a tweak derived
	// from the per-commitment point to ensure unique keys for each
	// commitment transaction.
	DelayBasePoint *btcec.PublicKey
}

// OpenChannel encapsulates the persistent and dynamic state of an open channel
// with a remote node. An open channel supports several options for on-disk
// serialization depending on the exact context. Full (upon channel creation)
// state commitments, and partial (due to a commitment update) writes are
// supported. Each partial write due to a state update appends the new update
// to an on-disk log, which can then subsequently be queried in order to
// "time-travel" to a prior state.
type OpenChannel struct {
	// ChanType denotes which type of channel this is.
	ChanType ChannelType

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

	// ShortChanID encodes the exact location in the chain in which the
	// channel was initially confirmed. This includes: the block height,
	// transaction index, and the output within the target transaction.
	ShortChanID lnwire.ShortChannelID

	// IsPending indicates whether a channel's funding transaction has been
	// confirmed.
	IsPending bool

	// IsInitiator is a bool which indicates if we were the original
	// initiator for the channel. This value may affect how higher levels
	// negotiate fees, or close the channel.
	IsInitiator bool

	// FundingBroadcastHeight is the height in which the funding
	// transaction was broadcast. This value can be used by higher level
	// sub-systems to determine if a channel is stale and/or should have
	// been confirmed before a certain height.
	FundingBroadcastHeight uint32

	// IdentityPub is the identity public key of the remote node this
	// channel has been established with.
	IdentityPub *btcec.PublicKey

	// LocalChanCfg is the channel configuration for the local node.
	LocalChanCfg ChannelConfig

	// RemoteChanCfg is the channel configuration for the remote node.
	RemoteChanCfg ChannelConfig

	// FeePerKw is the min satoshis/kilo-weight that should be paid within
	// the commitment transaction for the entire duration of the channel's
	// lifetime. This field may be updated during normal operation of the
	// channel as on-chain conditions change.
	FeePerKw btcutil.Amount

	// Capacity is the total capacity of this channel.
	Capacity btcutil.Amount

	// LocalBalance is the current available settled balance within the
	// channel directly spendable by us.
	LocalBalance lnwire.MilliSatoshi

	// RemoteBalance is the current available settled balance within the
	// channel directly spendable by the remote node.
	RemoteBalance lnwire.MilliSatoshi

	// CommitFee is the amount calculated to be paid in fees for the
	// current set of commitment transactions. The fee amount is persisted
	// with the channel in order to allow the fee amount to be removed and
	// recalculated with each channel state update, including updates that
	// happen after a system restart.
	CommitFee btcutil.Amount

	// CommitKey is the latest version of the commitment state, broadcast
	// able by us.
	CommitTx wire.MsgTx

	// CommitSig is one half of the signature required to fully complete
	// the script for the commitment transaction above. This is the
	// signature signed by the remote party for our version of the
	// commitment transactions.
	CommitSig []byte

	// NumConfsRequired is the number of confirmations a channel's funding
	// transaction must have received in order to be considered available
	// for normal transactional use.
	NumConfsRequired uint16

	// RemoteCurrentRevocation is the current revocation for their
	// commitment transaction. However, since this the derived public key,
	// we don't yet have the private key so we aren't yet able to verify
	// that it's actually in the hash chain.
	RemoteCurrentRevocation *btcec.PublicKey

	// RemoteNextRevocation is the revocation key to be used for the *next*
	// commitment transaction we create for the local node. Within the
	// specification, this value is referred to as the
	// per-commitment-point.
	RemoteNextRevocation *btcec.PublicKey

	// RevocationProducer is used to generate the revocation in such a way
	// that remote side might store it efficiently and have the ability to
	// restore the revocation by index if needed. Current implementation of
	// secret producer is shachain producer.
	RevocationProducer shachain.Producer

	// RevocationStore is used to efficiently store the revocations for
	// previous channels states sent to us by remote side. Current
	// implementation of secret store is shachain store.
	RevocationStore shachain.Store

	// NumUpdates is the total number of updates conducted within this
	// channel.
	NumUpdates uint64

	// TotalMSatSent is the total number of milli-satoshis we've sent
	// within this channel.
	TotalMSatSent lnwire.MilliSatoshi

	// TotalMSatReceived is the total number of milli-satoshis we've
	// received within this channel.
	TotalMSatReceived lnwire.MilliSatoshi

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
	chanIndexBucket, err := nodeChanBucket.CreateBucketIfNotExists(chanIDBucket)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if err := writeOutpoint(&b, &c.FundingOutpoint); err != nil {
		return err
	}
	if chanIndexBucket.Get(b.Bytes()) == nil {
		if err := chanIndexBucket.Put(b.Bytes(), nil); err != nil {
			return err
		}
	}

	return putOpenChannel(chanBucket, nodeChanBucket, c)
}

// SyncPending writes the contents of the channel to the database while it's in
// the pending (waiting for funding confirmation) state. The IsPending flag
// will be set to true. When the channel's funding transaction is confirmed,
// the channel should be marked as "open" and the IsPending flag set to false.
// Note that this function also creates a LinkNode relationship between this
// newly created channel and a new LinkNode instance. This allows listing all
// channels in the database globally, or according to the LinkNode they were
// created with.
//
// TODO(roasbeef): addr param should eventually be a lnwire.NetAddress type
// that includes service bits.
func (c *OpenChannel) SyncPending(addr *net.TCPAddr, pendingHeight uint32) error {
	c.Lock()
	defer c.Unlock()

	c.FundingBroadcastHeight = pendingHeight

	return c.Db.Update(func(tx *bolt.Tx) error {
		// First, sync all the persistent channel state to disk.
		if err := c.fullSync(tx); err != nil {
			return err
		}

		nodeInfoBucket, err := tx.CreateBucketIfNotExists(nodeInfoBucket)
		if err != nil {
			return err
		}

		// If a LinkNode for this identity public key already exists,
		// then we can exit early.
		nodePub := c.IdentityPub.SerializeCompressed()
		if nodeInfoBucket.Get(nodePub) != nil {
			return nil
		}

		// Next, we need to establish a (possibly) new LinkNode
		// relationship for this channel. The LinkNode metadata
		// contains reachability, up-time, and service bits related
		// information.
		// TODO(roasbeef): net info should be in lnwire.NetAddress
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
		c.CommitTx = *newCommitment
		c.CommitSig = newSig
		c.LocalBalance = delta.LocalBalance
		c.RemoteBalance = delta.RemoteBalance
		c.NumUpdates = delta.UpdateNum
		c.Htlcs = delta.Htlcs
		c.CommitFee = delta.CommitFee
		c.FeePerKw = delta.FeePerKw

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
		if err := putChanCommitFee(chanBucket, c); err != nil {
			return err
		}
		if err := putChanFeePerKw(chanBucket, c); err != nil {
			return err
		}
		if err := putChanCommitTxns(nodeChanBucket, c); err != nil {
			return err
		}
		if err := putCurrentHtlcs(nodeChanBucket, delta.Htlcs,
			&c.FundingOutpoint); err != nil {
			return err
		}

		return nil
	})
}

// HTLC is the on-disk representation of a hash time-locked contract. HTLCs
// are contained within ChannelDeltas which encode the current state of the
// commitment between state updates.
type HTLC struct {
	// Signature is the signature for the second level covenant transaction
	// for this HTLC. The second level transaction is a timeout tx in the
	// case that this is an outgoing HTLC, and a success tx in the case
	// that this is an incoming HTLC.
	//
	// TODO(roasbeef): make [64]byte instead?
	Signature []byte

	// RHash is the payment hash of the HTLC.
	RHash [32]byte

	// Amt is the amount of milli-satoshis this HTLC escrows.
	Amt lnwire.MilliSatoshi

	// RefundTimeout is the absolute timeout on the HTLC that the sender
	// must wait before reclaiming the funds in limbo.
	RefundTimeout uint32

	// OutputIndex is the output index for this particular HTLC output
	// within the commitment transaction.
	OutputIndex int32

	// Incoming denotes whether we're the receiver or the sender of this
	// HTLC.
	Incoming bool

	// OnionBlob is an opaque blob which is used to complete multi-hop
	// routing.
	OnionBlob []byte
}

// Copy returns a full copy of the target HTLC.
func (h *HTLC) Copy() HTLC {
	clone := HTLC{
		Incoming:      h.Incoming,
		Amt:           h.Amt,
		RefundTimeout: h.RefundTimeout,
		OutputIndex:   h.OutputIndex,
	}
	copy(clone.Signature[:], h.Signature)
	copy(clone.RHash[:], h.RHash[:])

	return clone
}

// ChannelDelta is a snapshot of the commitment state at a particular point in
// the commitment chain. With each state transition, a snapshot of the current
// state along with all non-settled HTLCs are recorded. These snapshots detail
// the state of the _remote_ party's commitment at a particular state number.
// For ourselves (the local node) we ONLY store our most recent (unrevoked)
// state for safety purposes.
type ChannelDelta struct {
	// LocalBalance is our current balance at this particular update
	// number.
	LocalBalance lnwire.MilliSatoshi

	// RemoteBalanceis the balance of the remote node at this particular
	// update number.
	RemoteBalance lnwire.MilliSatoshi

	// CommitFee is the fee that has been subtracted from the channel
	// initiator's balance at this point in the commitment chain.
	CommitFee btcutil.Amount

	// FeePerKw is the fee per kw used to calculate the commit fee at this point
	// in the commit chain.
	FeePerKw btcutil.Amount

	// UpdateNum is the update number that this ChannelDelta represents the
	// total number of commitment updates to this point. This can be viewed
	// as sort of a "commitment height" as this number is monotonically
	// increasing.
	UpdateNum uint64

	// Htlcs is the set of HTLC's that are pending at this particular
	// commitment height.
	Htlcs []*HTLC
}

// InsertNextRevocation inserts the _next_ commitment point (revocation) into
// the database, and also modifies the internal RemoteNextRevocation attribute
// to point to the passed key. This method is to be using during final channel
// set up, _after_ the channel has been fully confirmed.
//
// NOTE: If this method isn't called, then the target channel won't be able to
// propose new states for the commitment state of the remote party.
func (c *OpenChannel) InsertNextRevocation(revKey *btcec.PublicKey) error {
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

		c.RemoteNextRevocation = revKey
		return putChanRevocationState(nodeChanBucket, c)
	})
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

		// Persist the latest preimage state to disk as the remote peer
		// has just added to our local preimage store, and
		// given us a new pending revocation key.
		if err := putChanRevocationState(nodeChanBucket, c); err != nil {
			return err
		}

		// With the current preimage producer/store state updated,
		// append a new log entry recording this the delta of this state
		// transition.
		// TODO(roasbeef): could make the deltas relative, would save
		// space, but then tradeoff for more disk-seeks to recover the
		// full state.
		logKey := channelLogBucket
		logBucket, err := nodeChanBucket.CreateBucketIfNotExists(logKey)
		if err != nil {
			return err
		}

		return appendChannelLogEntry(logBucket, delta, &c.FundingOutpoint)
	})
}

// RevocationLogTail returns the "tail", or the end of the current revocation
// log. This entry represents the last previous state for the remote node's
// commitment chain. The ChannelDelta returned by this method will always lag
// one state behind the most current (unrevoked) state of the remote node's
// commitment chain.
func (c *OpenChannel) RevocationLogTail() (*ChannelDelta, error) {
	// If we haven't created any state updates yet, then we'll exit erly as
	// there's nothing to be found on disk in the revocation bucket.
	if c.NumUpdates == 0 {
		return nil, nil
	}

	var delta *ChannelDelta
	if err := c.Db.View(func(tx *bolt.Tx) error {
		chanBucket := tx.Bucket(openChannelBucket)

		nodePub := c.IdentityPub.SerializeCompressed()
		nodeChanBucket := chanBucket.Bucket(nodePub)
		if nodeChanBucket == nil {
			return ErrNoActiveChannels
		}

		logBucket := nodeChanBucket.Bucket(channelLogBucket)
		if logBucket == nil {
			return ErrNoPastDeltas
		}

		// Once we have the bucket that stores the revocation log from
		// this channel, we'll jump to the _last_ key in bucket. As we
		// store the update number on disk in a big-endian format,
		// this'll retrieve the latest entry.
		cursor := logBucket.Cursor()
		_, tailLogEntry := cursor.Last()
		logEntryReader := bytes.NewReader(tailLogEntry)

		// Once we have the entry, we'll decode it into the channel
		// delta pointer we created above.
		var dbErr error
		delta, dbErr = deserializeChannelDelta(logEntryReader)
		if dbErr != nil {
			return dbErr
		}

		return nil
	}); err != nil {
		return nil, err
	}

	return delta, nil
}

// CommitmentHeight returns the current commitment height. The commitment
// height represents the number of updates to the commitment state to data.
// This value is always monotonically increasing. This method is provided in
// order to allow multiple instances of a particular open channel to obtain a
// consistent view of the number of channel updates to data.
func (c *OpenChannel) CommitmentHeight() (uint64, error) {
	// TODO(roasbeef): this is super hacky, remedy during refactor!!!
	o := &OpenChannel{
		FundingOutpoint: c.FundingOutpoint,
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
		if logBucket == nil {
			return ErrNoPastDeltas
		}

		var err error
		delta, err = fetchChannelLogEntry(logBucket, &c.FundingOutpoint,
			updateNum)

		return err
	})
	if err != nil {
		return nil, err
	}

	return delta, nil
}

// ClosureType is an enum like structure that details exactly _how_ a channel
// was closed. Three closure types are currently possible: cooperative, force,
// and breach.
type ClosureType uint8

const (
	// CooperativeClose indicates that a channel has been closed
	// cooperatively.  This means that both channel peers were online and
	// signed a new transaction paying out the settled balance of the
	// contract.
	CooperativeClose ClosureType = iota

	// ForceClose indicates that one peer unilaterally broadcast their
	// current commitment state on-chain.
	ForceClose

	// BreachClose indicates that one peer attempted to broadcast a prior
	// _revoked_ channel state.
	BreachClose

	// FundingCanceled indicates that the channel never was fully opened before it
	// was marked as closed in the database. This can happen if we or the remote
	// fail at some point during the opening workflow, or we timeout waiting for
	// the funding transaction to be confirmed.
	FundingCanceled
)

// ChannelCloseSummary contains the final state of a channel at the point it
// was closed. Once a channel is closed, all the information pertaining to
// that channel within the openChannelBucket is deleted, and a compact
// summary is put in place instead.
type ChannelCloseSummary struct {
	// ChanPoint is the outpoint for this channel's funding transaction,
	// and is used as a unique identifier for the channel.
	ChanPoint wire.OutPoint

	// ClosingTXID is the txid of the transaction which ultimately closed
	// this channel.
	ClosingTXID chainhash.Hash

	// RemotePub is the public key of the remote peer that we formerly had
	// a channel with.
	RemotePub *btcec.PublicKey

	// Capacity was the total capacity of the channel.
	Capacity btcutil.Amount

	// SettledBalance is our total balance settled balance at the time of
	// channel closure. This _does not_ include the sum of any outputs that
	// have been time-locked as a result of the unilateral channel closure.
	SettledBalance btcutil.Amount

	// TimeLockedBalance is the sum of all the time-locked outputs at the
	// time of channel closure. If we triggered the force closure of this
	// channel, then this value will be non-zero if our settled output is
	// above the dust limit. If we were on the receiving side of a channel
	// force closure, then this value will be non-zero if we had any
	// outstanding outgoing HTLC's at the time of channel closure.
	TimeLockedBalance btcutil.Amount

	// CloseType details exactly _how_ the channel was closed. Three
	// closure types are possible: cooperative, force, and breach.
	CloseType ClosureType

	// IsPending indicates whether this channel is in the 'pending close'
	// state, which means the channel closing transaction has been
	// broadcast, but not confirmed yet or has not yet been fully resolved.
	// In the case of a channel that has been cooperatively closed, it will
	// no longer be considered pending as soon as the closing transaction
	// has been confirmed. However, for channel that have been force
	// closed, they'll stay marked as "pending" until _all_ the pending
	// funds have been swept.
	IsPending bool

	// TODO(roasbeef): also store short_chan_id?
}

// CloseChannel closes a previously active lightning channel. Closing a channel
// entails deleting all saved state within the database concerning this
// channel. This method also takes a struct that summarizes the state of the
// channel at closing, this compact representation will be the only component
// of a channel left over after a full closing.
func (c *OpenChannel) CloseChannel(summary *ChannelCloseSummary) error {
	return c.Db.Update(func(tx *bolt.Tx) error {
		// First fetch the top level bucket which stores all data
		// related to current, active channels.
		chanBucket := tx.Bucket(openChannelBucket)
		if chanBucket == nil {
			return ErrNoChanDBExists
		}

		// Within this top level bucket, fetch the bucket dedicated to
		// storing open channel data specific to the remote node.
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

		var b bytes.Buffer
		if err := writeOutpoint(&b, &c.FundingOutpoint); err != nil {
			return err
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
			outPointBytes, &c.FundingOutpoint); err != nil {
			return err
		}

		// With the base channel data deleted, attempt to delte the
		// information stored within the revocation log.
		logBucket := nodeChanBucket.Bucket(channelLogBucket)
		if logBucket != nil {
			err := wipeChannelLogEntries(logBucket, &c.FundingOutpoint)
			if err != nil {
				return err
			}
		}

		// Finally, create a summary of this channel in the closed
		// channel bucket for this node.
		return putChannelCloseSummary(tx, outPointBytes, summary)
	})
}

// ChannelSnapshot is a frozen snapshot of the current channel state. A
// snapshot is detached from the original channel that generated it, providing
// read-only access to the current or prior state of an active channel.
type ChannelSnapshot struct {
	// RemoteIdentity is the identity public key of the remote node that we
	// are maintaining the open channel with.
	RemoteIdentity btcec.PublicKey

	// ChannelPoint is the channel point that uniquly identifies the
	// channel whose delta this is.
	ChannelPoint wire.OutPoint

	// Capacity is the total capacity of the channel in satoshis.
	Capacity btcutil.Amount

	// LocalBalance is the amount of mSAT allocated to the local party.
	LocalBalance lnwire.MilliSatoshi

	// RemoteBalance is the amount of mSAT allocated to the remote party.
	RemoteBalance lnwire.MilliSatoshi

	// NumUpdates is the number of updates that have taken place within the
	// commitment transaction itself.
	NumUpdates uint64

	// CommitFee is the total fee paid on the commitment transaction at
	// this current commitment state.
	CommitFee btcutil.Amount

	// TotalMilliSatoshisSent is the total number of mSAT sent by the local
	// party at this current commitment instance.
	TotalMilliSatoshisSent lnwire.MilliSatoshi

	// TotalMilliSatoshisReceived is the total number of mSAT received by
	// the local party current commitment instance.
	TotalMilliSatoshisReceived lnwire.MilliSatoshi

	// Htlcs is the current set of outstanding HTLC's live on the
	// commitment transaction at this instance.
	Htlcs []HTLC
}

// Snapshot returns a read-only snapshot of the current channel state. This
// snapshot includes information concerning the current settled balance within
// the channel, metadata detailing total flows, and any outstanding HTLCs.
func (c *OpenChannel) Snapshot() *ChannelSnapshot {
	c.RLock()
	defer c.RUnlock()

	snapshot := &ChannelSnapshot{
		RemoteIdentity:             *c.IdentityPub,
		ChannelPoint:               c.FundingOutpoint,
		Capacity:                   c.Capacity,
		LocalBalance:               c.LocalBalance,
		RemoteBalance:              c.RemoteBalance,
		NumUpdates:                 c.NumUpdates,
		CommitFee:                  c.CommitFee,
		TotalMilliSatoshisSent:     c.TotalMSatSent,
		TotalMilliSatoshisReceived: c.TotalMSatReceived,
	}

	// Copy over the current set of HTLCs to ensure the caller can't
	// mutate our internal state.
	snapshot.Htlcs = make([]HTLC, len(c.Htlcs))
	for i, h := range c.Htlcs {
		snapshot.Htlcs[i] = h.Copy()
	}

	return snapshot
}

func putChannelCloseSummary(tx *bolt.Tx, chanID []byte,
	summary *ChannelCloseSummary) error {

	closedChanBucket, err := tx.CreateBucketIfNotExists(closedChannelBucket)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := serializeChannelCloseSummary(&b, summary); err != nil {
		return err
	}

	return closedChanBucket.Put(chanID, b.Bytes())
}

func serializeChannelCloseSummary(w io.Writer, cs *ChannelCloseSummary) error {
	if err := binary.Write(w, byteOrder, cs.IsPending); err != nil {
		return err
	}

	if err := writeOutpoint(w, &cs.ChanPoint); err != nil {
		return err
	}
	if _, err := w.Write(cs.ClosingTXID[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, cs.SettledBalance); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, cs.TimeLockedBalance); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, cs.Capacity); err != nil {
		return err
	}

	if _, err := w.Write([]byte{byte(cs.CloseType)}); err != nil {
		return err
	}

	pub := cs.RemotePub.SerializeCompressed()
	if _, err := w.Write(pub); err != nil {
		return err
	}

	return nil
}

func fetchChannelCloseSummary(tx *bolt.Tx,
	chanID []byte) (*ChannelCloseSummary, error) {

	closedChanBucket, err := tx.CreateBucketIfNotExists(closedChannelBucket)
	if err != nil {
		return nil, err
	}

	summaryBytes := closedChanBucket.Get(chanID)
	if summaryBytes == nil {
		return nil, fmt.Errorf("closed channel summary not found")
	}

	summaryReader := bytes.NewReader(summaryBytes)
	return deserializeCloseChannelSummary(summaryReader)
}

func deserializeCloseChannelSummary(r io.Reader) (*ChannelCloseSummary, error) {
	c := &ChannelCloseSummary{}

	var err error

	if err := binary.Read(r, byteOrder, &c.IsPending); err != nil {
		return nil, err
	}

	if err := readOutpoint(r, &c.ChanPoint); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(r, c.ClosingTXID[:]); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &c.SettledBalance); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &c.TimeLockedBalance); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &c.Capacity); err != nil {
		return nil, err
	}

	var closeType [1]byte
	if err := binary.Read(r, byteOrder, closeType[:]); err != nil {
		return nil, err
	}
	c.CloseType = ClosureType(closeType[0])

	var pub [33]byte
	if _, err := io.ReadFull(r, pub[:]); err != nil {
		return nil, err
	}
	c.RemotePub, err = btcec.ParsePubKey(pub[:], btcec.S256())
	if err != nil {
		return nil, err
	}

	return c, nil
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
	if err := putChanFeePerKw(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNumUpdates(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanAmountsTransferred(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanIsPending(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanConfInfo(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitFee(openChanBucket, channel); err != nil {
		return err
	}

	// Next, write out the fields of the channel update less frequently.
	if err := putChannelIDs(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanConfigs(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitTxns(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanFundingInfo(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanRevocationState(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putCurrentHtlcs(nodeChanBucket, channel.Htlcs,
		&channel.FundingOutpoint); err != nil {
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
		FundingOutpoint: *chanID,
	}

	// First, read out the fields of the channel update less frequently.
	if err = fetchChannelIDs(nodeChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read chan ID's: %v", err)
	}
	if err = fetchChanConfigs(nodeChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read chan config: %v", err)
	}
	if err = fetchChanCommitTxns(nodeChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read commit txns: %v", err)
	}
	if err = fetchChanFundingInfo(nodeChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read funding info: %v", err)
	}
	if err = fetchChanRevocationState(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	channel.Htlcs, err = fetchCurrentHtlcs(nodeChanBucket, chanID)
	if err != nil {
		return nil, fmt.Errorf("unable to read current htlc's: %v", err)
	}

	// With the existence of an open channel bucket with this node verified,
	// perform a full read of the entire struct. Starting with the prefixed
	// fields residing in the parent bucket.
	// TODO(roasbeef): combine the below into channel config like key
	if err = fetchChanCapacity(openChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read chan capacity: %v", err)
	}
	if err = fetchChanMinFeePerKw(openChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read fee-per-kb: %v", err)
	}
	if err = fetchChanNumUpdates(openChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read num updates: %v", err)
	}
	if err = fetchChanAmountsTransferred(openChanBucket, channel); err != nil {
		return nil, fmt.Errorf("unable to read sat transferred: %v", err)
	}
	if err = fetchChanIsPending(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanConfInfo(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err = fetchChanCommitFee(openChanBucket, channel); err != nil {
		return nil, err
	}

	return channel, nil
}

func deleteOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channelID []byte, o *wire.OutPoint) error {

	// First we'll delete all the "common" top level items stored outside
	// the node's channel bucket.
	if err := deleteChanCapacity(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanMinFeePerKw(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanNumUpdates(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanAmountsTransferred(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanIsPending(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanConfInfo(openChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitFee(openChanBucket, channelID); err != nil {
		return err
	}

	// Finally, delete all the fields directly within the node's channel
	// bucket.
	if err := deleteChannelIDs(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanConfigs(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanCommitTxns(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanFundingInfo(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteChanRevocationState(nodeChanBucket, channelID); err != nil {
		return err
	}
	if err := deleteCurrentHtlcs(nodeChanBucket, o); err != nil {
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
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
	byteOrder.PutUint64(scratch2, uint64(channel.LocalBalance))
	if err := openChanBucket.Put(keyPrefix, scratch2); err != nil {
		return err
	}

	copy(keyPrefix[:3], theirBalancePrefix)
	byteOrder.PutUint64(scratch3, uint64(channel.RemoteBalance))
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], chanCapacityPrefix)
	capacityBytes := openChanBucket.Get(keyPrefix)
	channel.Capacity = btcutil.Amount(byteOrder.Uint64(capacityBytes))

	copy(keyPrefix[:3], selfBalancePrefix)
	selfBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.LocalBalance = lnwire.MilliSatoshi(byteOrder.Uint64(selfBalanceBytes))

	copy(keyPrefix[:3], theirBalancePrefix)
	theirBalanceBytes := openChanBucket.Get(keyPrefix)
	channel.RemoteBalance = lnwire.MilliSatoshi(byteOrder.Uint64(theirBalanceBytes))

	return nil
}

func putChanFeePerKw(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.FeePerKw))

	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKwPrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanMinFeePerKw(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix, minFeePerKwPrefix)
	copy(keyPrefix[3:], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanMinFeePerKw(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, minFeePerKwPrefix)
	copy(keyPrefix[3:], b.Bytes())

	feeBytes := openChanBucket.Get(keyPrefix)
	channel.FeePerKw = btcutil.Amount(byteOrder.Uint64(feeBytes))

	return nil
}

func putChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, channel.NumUpdates)

	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.TotalMSatSent))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], satReceivedPrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.TotalMSatReceived))
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())

	copy(keyPrefix[:3], satSentPrefix)
	totalSentBytes := openChanBucket.Get(keyPrefix)
	channel.TotalMSatSent = lnwire.MilliSatoshi(byteOrder.Uint64(totalSentBytes))

	copy(keyPrefix[:3], satReceivedPrefix)
	totalReceivedBytes := openChanBucket.Get(keyPrefix)
	channel.TotalMSatReceived = lnwire.MilliSatoshi(byteOrder.Uint64(totalReceivedBytes))

	return nil
}

func putChanIsPending(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 2)

	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())
	copy(keyPrefix[:3], isPendingPrefix)

	if channel.IsPending {
		byteOrder.PutUint16(scratch, uint16(1))
		return openChanBucket.Put(keyPrefix, scratch)
	}

	byteOrder.PutUint16(scratch, uint16(0))
	return openChanBucket.Put(keyPrefix, scratch)
}

func deleteChanIsPending(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, 3+len(chanID))
	copy(keyPrefix[3:], chanID)
	copy(keyPrefix[:3], isPendingPrefix)
	return openChanBucket.Delete(keyPrefix)
}

func fetchChanIsPending(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix[3:], b.Bytes())
	copy(keyPrefix[:3], isPendingPrefix)

	isPending := byteOrder.Uint16(openChanBucket.Get(keyPrefix))
	if isPending == 1 {
		channel.IsPending = true
	} else {
		channel.IsPending = false
	}

	return nil
}

func putChanConfInfo(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, len(confInfoPrefix)+b.Len())
	copy(keyPrefix[:len(confInfoPrefix)], confInfoPrefix)
	copy(keyPrefix[len(confInfoPrefix):], b.Bytes())

	// We store the conf info in the following format: broadcast || open.
	var scratch [12]byte
	byteOrder.PutUint32(scratch[:], channel.FundingBroadcastHeight)
	byteOrder.PutUint64(scratch[4:], channel.ShortChanID.ToUint64())

	return openChanBucket.Put(keyPrefix, scratch[:])
}

func fetchChanConfInfo(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, len(confInfoPrefix)+b.Len())
	copy(keyPrefix[:len(confInfoPrefix)], confInfoPrefix)
	copy(keyPrefix[len(confInfoPrefix):], b.Bytes())

	confInfoBytes := openChanBucket.Get(keyPrefix)
	channel.FundingBroadcastHeight = byteOrder.Uint32(confInfoBytes[:4])
	channel.ShortChanID = lnwire.NewShortChanIDFromInt(
		byteOrder.Uint64(confInfoBytes[4:]),
	)

	return nil
}

func deleteChanConfInfo(openChanBucket *bolt.Bucket, chanID []byte) error {
	keyPrefix := make([]byte, len(confInfoPrefix)+len(chanID))
	copy(keyPrefix[:len(confInfoPrefix)], confInfoPrefix)
	copy(keyPrefix[len(confInfoPrefix):], chanID)
	return openChanBucket.Delete(keyPrefix)
}

func putChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// TODO(roasbeef): just pass in chanID everywhere for puts
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
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

	if err = writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
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

func putChanCommitFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.CommitFee))

	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, commitFeePrefix)
	copy(keyPrefix[3:], b.Bytes())

	return openChanBucket.Put(keyPrefix, scratch)
}

func fetchChanCommitFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}

	keyPrefix := make([]byte, 3+b.Len())
	copy(keyPrefix, commitFeePrefix)
	copy(keyPrefix[3:], b.Bytes())

	commitFeeBytes := openChanBucket.Get(keyPrefix)
	channel.CommitFee = btcutil.Amount(byteOrder.Uint64(commitFeeBytes))

	return nil
}

func deleteChanCommitFee(openChanBucket *bolt.Bucket, chanID []byte) error {
	commitFeeKey := make([]byte, 3+len(chanID))
	copy(commitFeeKey, commitFeePrefix)
	copy(commitFeeKey[3:], chanID)

	return openChanBucket.Delete(commitFeeKey)
}

func putChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	var b bytes.Buffer

	if err := channel.CommitTx.Serialize(&b); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.CommitSig); err != nil {
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
	if err = writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}
	txnsKey := make([]byte, len(commitTxnsKey)+bc.Len())
	copy(txnsKey[:3], commitTxnsKey)
	copy(txnsKey[3:], bc.Bytes())

	txnBytes := bytes.NewReader(nodeChanBucket.Get(txnsKey))

	channel.CommitTx = *wire.NewMsgTx(2)
	if err = channel.CommitTx.Deserialize(txnBytes); err != nil {
		return err
	}

	channel.CommitSig, err = wire.ReadVarBytes(txnBytes, 0, 80, "")
	if err != nil {
		return err
	}

	return nil
}

func putChanConfigs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer

	putChanConfig := func(cfg *ChannelConfig) error {
		err := binary.Write(&b, byteOrder, cfg.DustLimit)
		if err != nil {
			return err
		}
		err = binary.Write(&b, byteOrder, cfg.MaxPendingAmount)
		if err != nil {
			return err
		}
		err = binary.Write(&b, byteOrder, cfg.ChanReserve)
		if err != nil {
			return err
		}
		err = binary.Write(&b, byteOrder, cfg.MinHTLC)
		if err != nil {
			return err
		}
		err = binary.Write(&b, byteOrder, cfg.CsvDelay)
		if err != nil {
			return err
		}
		err = binary.Write(&b, byteOrder, cfg.MaxAcceptedHtlcs)
		if err != nil {
			return err
		}

		_, err = b.Write(cfg.MultiSigKey.SerializeCompressed())
		if err != nil {
			return err
		}
		_, err = b.Write(cfg.RevocationBasePoint.SerializeCompressed())
		if err != nil {
			return err
		}
		_, err = b.Write(cfg.PaymentBasePoint.SerializeCompressed())
		if err != nil {
			return err
		}
		_, err = b.Write(cfg.DelayBasePoint.SerializeCompressed())
		if err != nil {
			return err
		}

		return nil
	}

	putChanConfig(&channel.LocalChanCfg)
	putChanConfig(&channel.RemoteChanCfg)

	var bc bytes.Buffer
	if err := writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}
	configKey := make([]byte, len(chanConfigPrefix)+len(bc.Bytes()))
	copy(configKey, chanConfigPrefix)
	copy(configKey, bc.Bytes())

	return nodeChanBucket.Put(configKey, b.Bytes())
}

func fetchChanConfigs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}
	configKey := make([]byte, len(chanConfigPrefix)+len(bc.Bytes()))
	copy(configKey, chanConfigPrefix)
	copy(configKey, bc.Bytes())

	configBytes := nodeChanBucket.Get(configKey)
	if configBytes == nil {
		return fmt.Errorf("unable to find channel config for %v: ",
			channel.FundingOutpoint)
	}
	configReader := bytes.NewReader(configBytes)

	fetchChanConfig := func() (*ChannelConfig, error) {
		cfg := &ChannelConfig{}

		err := binary.Read(configReader, byteOrder, &cfg.DustLimit)
		if err != nil {
			return nil, err
		}
		err = binary.Read(configReader, byteOrder, &cfg.MaxPendingAmount)
		if err != nil {
			return nil, err
		}
		err = binary.Read(configReader, byteOrder, &cfg.ChanReserve)
		if err != nil {
			return nil, err
		}
		err = binary.Read(configReader, byteOrder, &cfg.MinHTLC)
		if err != nil {
			return nil, err
		}
		err = binary.Read(configReader, byteOrder, &cfg.CsvDelay)
		if err != nil {
			return nil, err
		}
		err = binary.Read(configReader, byteOrder, &cfg.MaxAcceptedHtlcs)
		if err != nil {
			return nil, err
		}

		var pub [33]byte
		readKey := func() (*btcec.PublicKey, error) {
			if _, err := io.ReadFull(configReader, pub[:]); err != nil {
				return nil, err
			}
			return btcec.ParsePubKey(pub[:], btcec.S256())
		}

		cfg.MultiSigKey, err = readKey()
		if err != nil {
			return nil, err
		}
		cfg.RevocationBasePoint, err = readKey()
		if err != nil {
			return nil, err
		}
		cfg.PaymentBasePoint, err = readKey()
		if err != nil {
			return nil, err
		}
		cfg.DelayBasePoint, err = readKey()
		if err != nil {
			return nil, err
		}

		return cfg, nil
	}

	var err error
	cfg, err := fetchChanConfig()
	if err != nil {
		return err
	}
	channel.LocalChanCfg = *cfg

	cfg, err = fetchChanConfig()
	if err != nil {
		return err
	}
	channel.RemoteChanCfg = *cfg

	return nil
}

func deleteChanConfigs(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	configKey := make([]byte, len(chanConfigPrefix)+len(chanID))
	copy(configKey, chanConfigPrefix)
	copy(configKey, chanID)
	return nodeChanBucket.Delete(configKey)
}

func putChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var bc bytes.Buffer
	if err := writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+bc.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], bc.Bytes())

	var b bytes.Buffer

	var boolByte [1]byte
	if channel.IsInitiator {
		boolByte[0] = 1
	} else {
		boolByte[0] = 0
	}

	if err := binary.Write(&b, byteOrder, boolByte[:]); err != nil {
		return err
	}

	// TODO(roasbeef): make first field instead?
	if _, err := b.Write([]byte{uint8(channel.ChanType)}); err != nil {
		return err
	}
	if _, err := b.Write(channel.ChainHash[:]); err != nil {
		return err
	}

	var scratch [2]byte
	byteOrder.PutUint16(scratch[:], channel.NumConfsRequired)
	if _, err := b.Write(scratch[:]); err != nil {
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
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}
	fundTxnKey := make([]byte, len(fundingTxnKey)+b.Len())
	copy(fundTxnKey[:3], fundingTxnKey)
	copy(fundTxnKey[3:], b.Bytes())

	infoBytes := bytes.NewReader(nodeChanBucket.Get(fundTxnKey))

	var err error
	var boolByte [1]byte
	err = binary.Read(infoBytes, byteOrder, boolByte[:])

	if err != nil {
		return err
	}
	if boolByte[0] == 1 {
		channel.IsInitiator = true
	} else {
		channel.IsInitiator = false
	}

	var chanType [1]byte
	err = binary.Read(infoBytes, byteOrder, chanType[:])

	if err != nil {
		return err
	}
	channel.ChanType = ChannelType(chanType[0])
	err = binary.Read(infoBytes, byteOrder, channel.ChainHash[:])

	if err != nil {
		return err
	}

	var scratch [2]byte
	if _, err := infoBytes.Read(scratch[:]); err != nil {
		return err
	}
	channel.NumConfsRequired = byteOrder.Uint16(scratch[:])

	return nil
}

func putChanRevocationState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer

	curRevKey := channel.RemoteCurrentRevocation.SerializeCompressed()
	if err := wire.WriteVarBytes(&b, 0, curRevKey); err != nil {
		return err
	}

	// TODO(roasbeef): shouldn't be storing on disk, should re-derive as
	// needed
	if err := channel.RevocationProducer.Encode(&b); err != nil {
		return err
	}
	if err := channel.RevocationStore.Encode(&b); err != nil {
		return err
	}

	var bc bytes.Buffer
	if err := writeOutpoint(&bc, &channel.FundingOutpoint); err != nil {
		return err
	}

	// We place the next revocation key at the very end, as under certain
	// circumstances (when a channel is initially funded), this value will
	// not yet have been set.
	//
	// TODO(roasbeef): segment the storage?
	if channel.RemoteNextRevocation != nil {
		nextRevKey := channel.RemoteNextRevocation.SerializeCompressed()
		if err := wire.WriteVarBytes(&b, 0, nextRevKey); err != nil {
			return err
		}
	}

	revocationKey := make([]byte, len(revocationStateKey)+bc.Len())
	copy(revocationKey[:3], revocationStateKey)
	copy(revocationKey[3:], bc.Bytes())
	return nodeChanBucket.Put(revocationKey, b.Bytes())
}

func deleteChanRevocationState(nodeChanBucket *bolt.Bucket, chanID []byte) error {
	revocationKey := make([]byte, len(revocationStateKey)+len(chanID))
	copy(revocationKey[:3], revocationStateKey)
	copy(revocationKey[3:], chanID)
	return nodeChanBucket.Delete(revocationKey)
}

func fetchChanRevocationState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := writeOutpoint(&b, &channel.FundingOutpoint); err != nil {
		return err
	}
	preimageKey := make([]byte, len(revocationStateKey)+b.Len())
	copy(preimageKey[:3], revocationStateKey)
	copy(preimageKey[3:], b.Bytes())

	reader := bytes.NewReader(nodeChanBucket.Get(preimageKey))

	curRevKeyBytes, err := wire.ReadVarBytes(reader, 0, 1000, "")
	if err != nil {
		return err
	}
	channel.RemoteCurrentRevocation, err = btcec.ParsePubKey(curRevKeyBytes, btcec.S256())
	if err != nil {
		return err
	}

	// TODO(roasbeef): should be rederiving on fly, or encrypting on disk.
	var root [32]byte
	if _, err := io.ReadFull(reader, root[:]); err != nil {
		return err
	}
	channel.RevocationProducer, err = shachain.NewRevocationProducerFromBytes(root[:])
	if err != nil {
		return err
	}

	channel.RevocationStore, err = shachain.NewRevocationStoreFromBytes(reader)
	if err != nil {
		return err
	}

	// We'll attempt to see if the remote party's next revocation key is
	// currently set, if so then we'll read and deserialize it. Otherwise,
	// we can exit early.
	if reader.Len() != 0 {
		nextRevKeyBytes, err := wire.ReadVarBytes(reader, 0, 1000, "")
		if err != nil {
			return err
		}
		channel.RemoteNextRevocation, err = btcec.ParsePubKey(
			nextRevKeyBytes, btcec.S256(),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func serializeHTLC(w io.Writer, h *HTLC) error {
	if err := wire.WriteVarBytes(w, 0, h.Signature); err != nil {
		return err
	}

	if _, err := w.Write(h.RHash[:]); err != nil {
		return err
	}

	if err := binary.Write(w, byteOrder, h.Amt); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, h.RefundTimeout); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, h.OutputIndex); err != nil {
		return err
	}

	var boolByte [1]byte
	if h.Incoming {
		boolByte[0] = 1
	} else {
		boolByte[0] = 0
	}

	if err := binary.Write(w, byteOrder, boolByte[:]); err != nil {
		return err
	}

	var onionLength [2]byte
	byteOrder.PutUint16(onionLength[:], uint16(len(h.OnionBlob)))
	if _, err := w.Write(onionLength[:]); err != nil {
		return err
	}

	if _, err := w.Write(h.OnionBlob); err != nil {
		return err
	}

	return nil
}

func deserializeHTLC(r io.Reader) (*HTLC, error) {
	h := &HTLC{}
	var err error

	h.Signature, err = wire.ReadVarBytes(r, 0, 80, "")
	if err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(r, h.RHash[:]); err != nil {
		return nil, err
	}

	if err := binary.Read(r, byteOrder, &h.Amt); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &h.RefundTimeout); err != nil {
		return nil, err
	}
	if err := binary.Read(r, byteOrder, &h.OutputIndex); err != nil {
		return nil, err
	}

	var scratch [1]byte
	if err := binary.Read(r, byteOrder, scratch[:]); err != nil {
		return nil, err
	}

	if boolByte[0] == 1 {
		h.Incoming = true
	} else {
		h.Incoming = false
	}

	var onionLength [2]byte
	if _, err := r.Read(onionLength[:]); err != nil {
		return nil, err
	}

	l := byteOrder.Uint16(onionLength[:])
	if l != 0 {
		h.OnionBlob = make([]byte, l)
		if _, err := io.ReadFull(r, h.OnionBlob); err != nil {
			return nil, err
		}
	}

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

func deleteCurrentHtlcs(nodeChanBucket *bolt.Bucket, o *wire.OutPoint) error {
	htlcKey := makeHtlcKey(o)
	return nodeChanBucket.Delete(htlcKey[:])
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

	byteOrder.PutUint64(scratch[:], delta.UpdateNum)
	if _, err := w.Write(scratch[:]); err != nil {
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

	byteOrder.PutUint64(scratch[:], uint64(delta.CommitFee))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
	}

	byteOrder.PutUint64(scratch[:], uint64(delta.FeePerKw))
	if _, err := w.Write(scratch[:]); err != nil {
		return err
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
	delta.LocalBalance = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.RemoteBalance = lnwire.MilliSatoshi(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.UpdateNum = byteOrder.Uint64(scratch[:])

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
	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.CommitFee = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	if _, err := r.Read(scratch[:]); err != nil {
		return nil, err
	}
	delta.FeePerKw = btcutil.Amount(byteOrder.Uint64(scratch[:]))

	return delta, nil
}

func makeLogKey(o *wire.OutPoint, updateNum uint64) [44]byte {
	var (
		scratch [8]byte
		n       int

		// txid (32) || index (4) || update_num (8)
		// 32 + 4 + 8 = 44
		k [44]byte
	)

	n += copy(k[:], o.Hash[:])

	byteOrder.PutUint32(scratch[:4], o.Index)
	n += copy(k[n:], scratch[:4])

	byteOrder.PutUint64(scratch[:], updateNum)
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
	updateNum uint64) (*ChannelDelta, error) {

	logEntrykey := makeLogKey(chanPoint, updateNum)
	deltaBytes := log.Get(logEntrykey[:])
	if deltaBytes == nil {
		return nil, fmt.Errorf("log entry not found")
	}

	deltaReader := bytes.NewReader(deltaBytes)

	return deserializeChannelDelta(deltaReader)
}

func wipeChannelLogEntries(log *bolt.Bucket, o *wire.OutPoint) error {
	var (
		n         int
		logPrefix [32 + 4]byte
		scratch   [4]byte
	)

	// First we'll construct a key prefix that we'll use to scan through
	// and delete all the log entries related to this channel. The format
	// for log entries within the database is: txid || index || update_num.
	// We'll construct a prefix key with the first two thirds of the full
	// key to scan with and delete all entries.
	n += copy(logPrefix[:], o.Hash[:])
	byteOrder.PutUint32(scratch[:], o.Index)
	copy(logPrefix[n:], scratch[:])

	// With the prefix constructed, scan through the log bucket from the
	// starting point of the log entries for this channel. We'll keep
	// deleting keys until the prefix no longer matches.
	logCursor := log.Cursor()
	for logKey, _ := logCursor.Seek(logPrefix[:]); bytes.HasPrefix(logKey, logPrefix[:]); logKey, _ = logCursor.Next() {
		if err := log.Delete(logKey); err != nil {
			return err
		}
	}

	return nil
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
	_, err := w.Write(scratch)
	return err
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
