package channeldb

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/LightningNetwork/lnd/elkrem"
	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/walletdb"
)

var (
	// openChanBucket stores all the currently open channels. This bucket
	// has a second, nested bucket which is keyed by a node's ID. Additionally,
	// at the base level of this bucket several prefixed keys are stored which
	// house channel meta-data such as total satoshis sent, number of updates
	// etc. These fields are stored at this top level rather than within a
	// node's channel bucket in orer to facilitate sequential prefix scans
	// to gather stats such as total satoshis received.
	openChannelBucket = []byte("ocb")

	// closedChannelBucket stores summarization information concerning
	// previously open, but now closed channels.
	closedChannelBucket = []byte("ccb")

	// channelLogBucket is dedicated for storing the necessary delta state
	// between channel updates required to re-construct a past state in
	// order to punish a counter party attempting a non-cooperative channel
	// closure.
	channelLogBucket = []byte("clb")

	// identityKey is the key for storing this node's current LD identity key.
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
	minFeePerKbPrefix  = []byte("mfp")
	updatePrefix       = []byte("uup")
	satSentPrefix      = []byte("ssp")
	satRecievedPrefix  = []byte("srp")
	netFeesPrefix      = []byte("ntp")

	// chanIDKey stores the node, and channelID for an active channel.
	chanIDKey = []byte("cik")

	// commitKeys stores both commitment keys (ours, and theirs) for an
	// active channel. Our private key is stored in an encrypted format
	// using channeldb's currently registered cryptoSystem.
	commitKeys = []byte("ckk")

	// commitTxnsKey stores the full version of both current, non-revoked
	// commitment transactions in addition to the csvDelay for both.
	commitTxnsKey = []byte("ctk")

	// fundingTxnKey stroes the funding tx, our encrypted multi-sig key,
	// and finally 2-of-2 multisig redeem script.
	fundingTxnKey = []byte("fsk")

	// elkremStateKey stores their current revocation hash, and our elkrem
	// sender, and their elkrem reciever.
	elkremStateKey = []byte("esk")

	// deliveryScriptsKey stores the scripts for the final delivery in the
	// case of a cooperative closure.
	deliveryScriptsKey = []byte("dsk")
)

// OpenChannel...
// TODO(roasbeef): Copy/Clone method, so CoW on writes?
//  * CoW method would allow for intelligent partial writes for updates
// TODO(roasbeef): UpdateState(func (newChan *OpenChannel) error)
//  * need mutex, invarient that all reads/writes grab the mutex
//  * needs to also return two slices of added then removed HTLC's
type OpenChannel struct {
	// Hash? or Their current pubKey?
	TheirLNID [wire.HashSize]byte

	// The ID of a channel is the txid of the funding transaction.
	ChanID [wire.HashSize]byte

	MinFeePerKb btcutil.Amount
	// Our reserve. Assume symmetric reserve amounts. Only needed if the
	// funding type is CLTV.
	//ReserveAmount btcutil.Amount

	// Keys for both sides to be used for the commitment transactions.
	OurCommitKey   *btcec.PrivateKey
	TheirCommitKey *btcec.PublicKey

	// Tracking total channel capacity, and the amount of funds allocated
	// to each side.
	Capacity     btcutil.Amount
	OurBalance   btcutil.Amount
	TheirBalance btcutil.Amount

	// Commitment transactions for both sides (they're asymmetric). Our
	// commitment transaction includes a valid sigScript, and is ready for
	// broadcast.
	TheirCommitTx *wire.MsgTx
	OurCommitTx   *wire.MsgTx

	// The final funding transaction. Kept for wallet-related records.
	FundingTx *wire.MsgTx

	MultiSigKey         *btcec.PrivateKey
	FundingRedeemScript []byte

	// In blocks
	LocalCsvDelay  uint32
	RemoteCsvDelay uint32

	// Current revocation for their commitment transaction. However, since
	// this is the hash, and not the pre-image, we can't yet verify that
	// it's actually in the chain.
	TheirCurrentRevocation [20]byte
	LocalElkrem            *elkrem.ElkremSender
	RemoteElkrem           *elkrem.ElkremReceiver

	// The pkScript for both sides to be used for final delivery in the case
	// of a cooperative close.
	OurDeliveryScript   []byte
	TheirDeliveryScript []byte

	NumUpdates            uint64
	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64
	TotalNetFees          uint64    // TODO(roasbeef): total fees paid too?
	CreationTime          time.Time // TODO(roasbeef): last update time?

	// isPrevState denotes if this instane of an OpenChannel is a previous,
	// revoked channel state. If so, then the FullSynv, and UpdateState
	// methods are disabled in order to prevent overiding the latest channel
	// state.
	// TODO(roasbeef): scrap? already have snapshots now?
	isPrevState bool

	// TODO(roasbeef): eww
	Db *DB

	sync.RWMutex
}

// ChannelSnapshot....
// TODO(roasbeef): methods to roll forwards/backwards in state etc
//  * use botldb cursor?
type ChannelSnapshot struct {
	OpenChannel

	// TODO(roasbeef): active HTLC's + their direction
	updateNum      uint64
	deltaNamespace walletdb.Namespace
}

// FindPreviousState...
// TODO(roasbeef): method to retrieve both old commitment txns given update #
func (c *OpenChannel) FindPreviousState(updateNum uint64) (*ChannelSnapshot, error) {
	return nil, nil
}

// Snapshot....
// read-only snapshot
func (c *OpenChannel) Snapshot() (*ChannelSnapshot, error) {
	return nil, nil
}

// ChannelDelta...
// TODO(roasbeef): binlog like entry?
type ChannelDelta struct {
	// change in allocations
	// added + removed htlcs
	// index
}

// RecordChannelDelta
// TODO(roasbeef): only need their commit?
//  * or as internal helper func to UpdateState func?
func (c OpenChannel) RecordChannelDelta(theirRevokedCommit *wire.MsgTx, updateNum uint64) error {
	// TODO(roasbeef): record all HTLCs, pass those instead?
	//  *
	return nil
}

// FullSync serializes, and writes to disk the *full* channel state, using
// both the active channel bucket to store the prefixed column fields, and the
// remote node's ID to store the remainder of the channel state.
//
// NOTE: This method requires an active EncryptorDecryptor to be registered in
// order to encrypt sensitive information.
func (c *OpenChannel) FullSync() error {
	return c.Db.store.Update(func(tx *bolt.Tx) error {
		// First fetch the top level bucket which stores all data related to
		// current, active channels.
		chanBucket := tx.Bucket(openChannelBucket)

		// WIthin this top level bucket, fetch the bucket dedicated to storing
		// open channel data specific to the remote node.
		nodeChanBucket, err := chanBucket.CreateBucketIfNotExists(c.TheirLNID[:])
		if err != nil {
			return err
		}

		return putOpenChannel(chanBucket, nodeChanBucket, c, c.Db.cryptoSystem)
	})
}

// putChannel serializes, and stores the current state of the channel in its
// entirety.
func putOpenChannel(openChanBucket *bolt.Bucket, nodeChanBucket *bolt.Bucket,
	channel *OpenChannel, encryptor EncryptorDecryptor) error {

	// First write out all the "common" fields using the field's prefix
	// appened with the channel's ID. These fields go into a top-level bucket
	// to allow for ease of metric aggregation via efficient prefix scans.
	if err := putChanCapacity(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanMinFeePerKb(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNumUpdates(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanTotalFlow(openChanBucket, channel); err != nil {
		return err
	}
	if err := putChanNetFee(openChanBucket, channel); err != nil {
		return err
	}

	// Next, write out the fields of the channel update less frequently.
	if err := putChannelIDs(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanCommitKeys(nodeChanBucket, channel, encryptor); err != nil {
		return err
	}
	if err := putChanCommitTxns(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanFundingInfo(nodeChanBucket, channel, encryptor); err != nil {
		return err
	}
	if err := putChanEklremState(nodeChanBucket, channel); err != nil {
		return err
	}
	if err := putChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return err
	}

	return nil
}

// fetchOpenChannel retrieves, and deserializes (including decrypting
// sensitive) the complete channel currently active with the passed nodeID.
// An EncryptorDecryptor is required to decrypt sensitive information stored
// within the database.
func fetchOpenChannel(openChanBucket *bolt.Bucket, nodeID [32]byte,
	decryptor EncryptorDecryptor) (*OpenChannel, error) {

	// WIthin this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodeChanBucket := openChanBucket.Bucket(nodeID[:])
	if nodeChanBucket == nil {
		return nil, fmt.Errorf("node chan bucket for node %v does not exist",
			hex.EncodeToString(nodeID[:]))
	}

	channel := &OpenChannel{}

	// First, read out the fields of the channel update less frequently.
	if err := fetchChannelIDs(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanCommitKeys(nodeChanBucket, channel, decryptor); err != nil {
		return nil, err
	}
	if err := fetchChanCommitTxns(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanFundingInfo(nodeChanBucket, channel, decryptor); err != nil {
		return nil, err
	}
	if err := fetchChanEklremState(nodeChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanDeliveryScripts(nodeChanBucket, channel); err != nil {
		return nil, err
	}

	// With the existence of an open channel bucket with this node verified,
	// perform a full read of the entire struct. Starting with the prefixed
	// fields residing in the parent bucket.
	if err := fetchChanCapacity(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanMinFeePerKb(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanNumUpdates(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanTotalFlow(openChanBucket, channel); err != nil {
		return nil, err
	}
	if err := fetchChanNetFee(openChanBucket, channel); err != nil {
		return nil, err
	}

	return channel, nil
}

func putChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// Some scratch bytes re-used for serializing each of the uint64's.
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)
	scratch3 := make([]byte, 8)

	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix[3:], channel.ChanID[:])

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

func fetchChanCapacity(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	// A byte slice re-used to compute each key prefix eblow.
	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix[3:], channel.ChanID[:])

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

	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], channel.ChanID[:])

	return openChanBucket.Put(keyPrefix, scratch)
}

func fetchChanMinFeePerKb(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix, minFeePerKbPrefix)
	copy(keyPrefix[3:], channel.ChanID[:])

	feeBytes := openChanBucket.Get(keyPrefix)
	channel.MinFeePerKb = btcutil.Amount(byteOrder.Uint64(feeBytes))

	return nil
}

func putChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, channel.NumUpdates)

	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], channel.ChanID[:])

	return openChanBucket.Put(keyPrefix, scratch)
}

func fetchChanNumUpdates(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix, updatePrefix)
	copy(keyPrefix[3:], channel.ChanID[:])

	updateBytes := openChanBucket.Get(keyPrefix)
	channel.NumUpdates = byteOrder.Uint64(updateBytes)

	return nil
}

func putChanTotalFlow(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch1 := make([]byte, 8)
	scratch2 := make([]byte, 8)
	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix[3:], channel.ChanID[:])

	copy(keyPrefix[:3], satSentPrefix)
	byteOrder.PutUint64(scratch1, uint64(channel.TotalSatoshisSent))
	if err := openChanBucket.Put(keyPrefix, scratch1); err != nil {
		return err
	}

	copy(keyPrefix[:3], satRecievedPrefix)
	byteOrder.PutUint64(scratch2, uint64(channel.TotalSatoshisReceived))
	return openChanBucket.Put(keyPrefix, scratch2)
}

func fetchChanTotalFlow(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	keyPrefix := make([]byte, 3+32)
	copy(keyPrefix[3:], channel.ChanID[:])

	copy(keyPrefix[:3], satSentPrefix)
	totalSentBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisSent = byteOrder.Uint64(totalSentBytes)

	copy(keyPrefix[:3], satRecievedPrefix)
	totalReceivedBytes := openChanBucket.Get(keyPrefix)
	channel.TotalSatoshisReceived = byteOrder.Uint64(totalReceivedBytes)

	return nil
}

func putChanNetFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	scratch := make([]byte, 8)
	keyPrefix := make([]byte, 3+32)

	copy(keyPrefix, netFeesPrefix)
	copy(keyPrefix[3:], channel.ChanID[:])
	byteOrder.PutUint64(scratch, uint64(channel.TotalNetFees))
	return openChanBucket.Put(keyPrefix, scratch)
}

func fetchChanNetFee(openChanBucket *bolt.Bucket, channel *OpenChannel) error {
	keyPrefix := make([]byte, 3+32)

	copy(keyPrefix, netFeesPrefix)
	copy(keyPrefix[3:], channel.ChanID[:])
	feeBytes := openChanBucket.Get(keyPrefix)
	channel.TotalNetFees = byteOrder.Uint64(feeBytes)

	return nil
}

func putChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	idSlice := make([]byte, wire.HashSize*2)
	copy(idSlice, channel.TheirLNID[:])
	copy(idSlice[32:], channel.ChanID[:])

	return nodeChanBucket.Put(chanIDKey, idSlice)
}

func fetchChannelIDs(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	idBytes := nodeChanBucket.Get(chanIDKey)

	copy(channel.TheirLNID[:], idBytes[:32])
	copy(channel.ChanID[:], idBytes[32:])

	return nil
}

func putChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel,
	ed EncryptorDecryptor) error {

	var b bytes.Buffer

	if _, err := b.Write(channel.TheirCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	encryptedPriv, err := ed.Encrypt(channel.OurCommitKey.Serialize())
	if err != nil {
		return err
	}

	if _, err := b.Write(encryptedPriv); err != nil {
		return err
	}

	return nodeChanBucket.Put(commitKeys, b.Bytes())
}

func fetchChanCommitKeys(nodeChanBucket *bolt.Bucket, channel *OpenChannel,
	ed EncryptorDecryptor) error {

	var err error
	keyBytes := nodeChanBucket.Get(commitKeys)

	channel.TheirCommitKey, err = btcec.ParsePubKey(keyBytes[:33], btcec.S256())
	if err != nil {
		return err
	}

	decryptedPriv, err := ed.Decrypt(keyBytes[33:])
	if err != nil {
		return err
	}

	channel.OurCommitKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)
	if err != nil {
		return err
	}

	return nil
}

func putChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer

	if err := channel.TheirCommitTx.Serialize(&b); err != nil {
		return err
	}

	if err := channel.OurCommitTx.Serialize(&b); err != nil {
		return err
	}

	scratch := make([]byte, 4)
	byteOrder.PutUint32(scratch, channel.LocalCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}
	byteOrder.PutUint32(scratch, channel.RemoteCsvDelay)
	if _, err := b.Write(scratch); err != nil {
		return err
	}

	return nodeChanBucket.Put(commitTxnsKey, b.Bytes())
}

func fetchChanCommitTxns(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	txnBytes := bytes.NewReader(nodeChanBucket.Get(commitTxnsKey))

	channel.TheirCommitTx = wire.NewMsgTx()
	if err := channel.TheirCommitTx.Deserialize(txnBytes); err != nil {
		return err
	}

	channel.OurCommitTx = wire.NewMsgTx()
	if err := channel.OurCommitTx.Deserialize(txnBytes); err != nil {
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

func putChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel,
	ed EncryptorDecryptor) error {
	var b bytes.Buffer

	if err := channel.FundingTx.Serialize(&b); err != nil {
		return err
	}

	encryptedPriv, err := ed.Encrypt(channel.MultiSigKey.Serialize())
	if err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, encryptedPriv); err != nil {
		return err
	}

	if err := wire.WriteVarBytes(&b, 0, channel.FundingRedeemScript[:]); err != nil {
		return err
	}

	scratch := make([]byte, 8)
	byteOrder.PutUint64(scratch, uint64(channel.CreationTime.Unix()))

	if _, err := b.Write(scratch); err != nil {
		return err
	}

	return nodeChanBucket.Put(fundingTxnKey, b.Bytes())
}

func fetchChanFundingInfo(nodeChanBucket *bolt.Bucket, channel *OpenChannel,
	ed EncryptorDecryptor) error {

	infoBytes := bytes.NewReader(nodeChanBucket.Get(fundingTxnKey))

	channel.FundingTx = wire.NewMsgTx()
	if err := channel.FundingTx.Deserialize(infoBytes); err != nil {
		return err
	}

	encryptedPrivBytes, err := wire.ReadVarBytes(infoBytes, 0, 100, "")
	if err != nil {
		return err
	}
	decryptedPriv, err := ed.Decrypt(encryptedPrivBytes)
	if err != nil {
		return err
	}
	channel.MultiSigKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)

	channel.FundingRedeemScript, err = wire.ReadVarBytes(infoBytes, 0, 520, "")
	if err != nil {
		return err
	}

	scratch := make([]byte, 8)
	if _, err := infoBytes.Read(scratch); err != nil {
		return err
	}
	unixSecs := byteOrder.Uint64(scratch)
	channel.CreationTime = time.Unix(int64(unixSecs), 0)

	return nil
}

func putChanEklremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer

	if _, err := b.Write(channel.TheirCurrentRevocation[:]); err != nil {
		return err
	}

	senderBytes, err := channel.LocalElkrem.ToBytes()
	if err != nil {
		return err
	}
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

	return nodeChanBucket.Put(elkremStateKey, b.Bytes())
}

func fetchChanEklremState(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	elkremStateBytes := bytes.NewReader(nodeChanBucket.Get(elkremStateKey))

	if _, err := elkremStateBytes.Read(channel.TheirCurrentRevocation[:]); err != nil {
		return err
	}

	senderBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	localE, err := elkrem.ElkremSenderFromBytes(senderBytes)
	if err != nil {
		return err
	}
	channel.LocalElkrem = &localE

	reciverBytes, err := wire.ReadVarBytes(elkremStateBytes, 0, 1000, "")
	if err != nil {
		return err
	}
	remoteE, err := elkrem.ElkremReceiverFromBytes(reciverBytes)
	if err != nil {
		return err
	}
	channel.RemoteElkrem = &remoteE

	return nil
}

func putChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
	var b bytes.Buffer
	if err := wire.WriteVarBytes(&b, 0, channel.OurDeliveryScript); err != nil {
		return err
	}
	if err := wire.WriteVarBytes(&b, 0, channel.TheirDeliveryScript); err != nil {
		return err
	}

	return nodeChanBucket.Put(deliveryScriptsKey, b.Bytes())

}

func fetchChanDeliveryScripts(nodeChanBucket *bolt.Bucket, channel *OpenChannel) error {
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
