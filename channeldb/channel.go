package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/boltdb/bolt"
	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/waddrmgr"
	"github.com/lightningnetwork/lnd/shachain"
)

var (
	openChannelBucket   = []byte("o")
	closedChannelBucket = []byte("c")
	activeChanKey       = []byte("a")

	identityKey = []byte("idkey")

	// TODO(roasbeef): replace w/ tesnet-L also revisit dependancy...
	ActiveNetParams = &chaincfg.TestNet3Params
)

// Payment...
type Payment struct {
	// r [32]byte
	// path *Route
}

// ClosedChannel...
type ClosedChannel struct {
}

// OpenChannel...
// TODO(roasbeef): store only the essentials? optimize space...
// TODO(roasbeef): switch to "column store"
type OpenChannel struct {
	// Hash? or Their current pubKey?
	// TODO(roasbeef): switch to Tadge's LNId
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
	OurCommitTx   *wire.MsgTx // TODO(roasbeef): store hash instead?

	// The final funding transaction. Kept wallet-related records.
	FundingTx *wire.MsgTx

	MultiSigKey         *btcec.PrivateKey
	FundingRedeemScript []byte

	// Current revocation for their commitment transaction. However, since
	// this is the hash, and not the pre-image, we can't yet verify that
	// it's actually in the chain.
	TheirCurrentRevocation [20]byte
	TheirShaChain          *shachain.HyperShaChain
	OurShaChain            *shachain.HyperShaChain

	// Final delivery address
	// TODO(roasbeef): should just be output scripts
	OurDeliveryAddress   btcutil.Address
	TheirDeliveryAddress btcutil.Address

	// In blocks
	CsvDelay uint32

	// TODO(roasbeef): track fees, other stats?
	NumUpdates            uint64
	TotalSatoshisSent     uint64
	TotalSatoshisReceived uint64
	CreationTime          time.Time
}

// PutOpenChannel...
func (d *DB) PutOpenChannel(channel *OpenChannel) error {
	return d.db.Update(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket, err := tx.CreateBucketIfNotExists(openChannelBucket)
		if err != nil {
			return err
		}

		return putOpenChannel(openChanBucket, channel, d.addrmgr)
	})
}

// GetOpenChannel...
// TODO(roasbeef): assumes only 1 active channel per-node
func (d *DB) FetchOpenChannel(nodeID [32]byte) (*OpenChannel, error) {
	var channel *OpenChannel

	err := d.db.View(func(tx *bolt.Tx) error {
		// Get the bucket dedicated to storing the meta-data for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChannelBucket == nil {
			return fmt.Errorf("open channel bucket does not exist")
		}

		oChannel, err := fetchOpenChannel(openChanBucket, nodeID,
			d.addrmgr)
		if err != nil {
			return err
		}
		channel = oChannel
		return nil
	})

	return channel, err
}

// putChannel...
func putOpenChannel(activeChanBucket *bolt.Bucket, channel *OpenChannel,
	addrmgr *waddrmgr.Manager) error {

	// Generate a serialized version of the open channel. The addrmgr is
	// required in order to encrypt densitive data.
	var b bytes.Buffer
	if err := channel.Encode(&b, addrmgr); err != nil {
		return err
	}

	// Grab the bucket dedicated to storing data related to this particular
	// node.
	nodeBucket, err := activeChanBucket.CreateBucketIfNotExists(channel.TheirLNID[:])
	if err != nil {
		return err
	}

	return nodeBucket.Put(activeChanKey, b.Bytes())
}

// fetchOpenChannel
func fetchOpenChannel(bucket *bolt.Bucket, nodeID [32]byte,
	addrmgr *waddrmgr.Manager) (*OpenChannel, error) {

	// Grab the bucket dedicated to storing data related to this particular
	// node.
	nodeBucket := bucket.Bucket(nodeID[:])
	if nodeBucket == nil {
		return nil, fmt.Errorf("channel bucket for node does not exist")
	}

	serializedChannel := nodeBucket.Get(activeChanKey)
	if serializedChannel == nil {
		// TODO(roasbeef): make proper in error.go
		return nil, fmt.Errorf("node has no open channels")
	}

	// Decode the serialized channel state, using the addrmgr to decrypt
	// sensitive information.
	channel := &OpenChannel{}
	reader := bytes.NewReader(serializedChannel)
	if err := channel.Decode(reader, addrmgr); err != nil {
		return nil, err
	}

	return channel, nil
}

// Encode...
// TODO(roasbeef): checksum
func (o *OpenChannel) Encode(b io.Writer, addrManager *waddrmgr.Manager) error {
	if _, err := b.Write(o.TheirLNID[:]); err != nil {
		return err
	}
	if _, err := b.Write(o.ChanID[:]); err != nil {
		return err
	}

	if err := binary.Write(b, endian, uint64(o.MinFeePerKb)); err != nil {
		return err
	}

	encryptedPriv, err := addrManager.Encrypt(waddrmgr.CKTPrivate,
		o.OurCommitKey.Serialize())
	if err != nil {
		return err
	}
	if _, err := b.Write(encryptedPriv); err != nil {
		return err
	}
	if _, err := b.Write(o.TheirCommitKey.SerializeCompressed()); err != nil {
		return err
	}

	if err := binary.Write(b, endian, uint64(o.Capacity)); err != nil {
		return err
	}
	if err := binary.Write(b, endian, uint64(o.OurBalance)); err != nil {
		return err
	}
	if err := binary.Write(b, endian, uint64(o.TheirBalance)); err != nil {
		return err
	}

	if err := o.TheirCommitTx.Serialize(b); err != nil {
		return err
	}
	if err := o.OurCommitTx.Serialize(b); err != nil {
		return err
	}

	if err := o.FundingTx.Serialize(b); err != nil {
		return err
	}

	encryptedPriv, err = addrManager.Encrypt(waddrmgr.CKTPrivate,
		o.MultiSigKey.Serialize())
	if err != nil {
		return err
	}
	if _, err := b.Write(encryptedPriv); err != nil {
		return err
	}
	if _, err := b.Write(o.FundingRedeemScript); err != nil {
		return err
	}

	if _, err := b.Write(o.TheirCurrentRevocation[:]); err != nil {
		return err
	}
	// TODO(roasbeef): serialize shachains

	if _, err := b.Write([]byte(o.OurDeliveryAddress.EncodeAddress())); err != nil {
		return err
	}
	if _, err := b.Write([]byte(o.TheirDeliveryAddress.EncodeAddress())); err != nil {
		return err
	}

	if err := binary.Write(b, endian, o.CsvDelay); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.NumUpdates); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.TotalSatoshisSent); err != nil {
		return err
	}
	if err := binary.Write(b, endian, o.TotalSatoshisReceived); err != nil {
		return err
	}

	if err := binary.Write(b, endian, o.CreationTime.Unix()); err != nil {
		return err
	}

	return nil
}

// Decode...
func (o *OpenChannel) Decode(b io.Reader, addrManager *waddrmgr.Manager) error {
	var scratch [8]byte

	if _, err := b.Read(o.TheirLNID[:]); err != nil {
		return err
	}
	if _, err := b.Read(o.ChanID[:]); err != nil {
		return err
	}

	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.MinFeePerKb = btcutil.Amount(endian.Uint64(scratch[:]))

	// nonce + serPrivKey + mac
	var encryptedPriv [24 + 32 + 16]byte
	if _, err := b.Read(encryptedPriv[:]); err != nil {
		return err
	}
	decryptedPriv, err := addrManager.Decrypt(waddrmgr.CKTPrivate, encryptedPriv[:])
	if err != nil {
		return err
	}
	o.OurCommitKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)

	var serPubKey [33]byte
	if _, err := b.Read(serPubKey[:]); err != nil {
		return err
	}
	o.TheirCommitKey, err = btcec.ParsePubKey(serPubKey[:], btcec.S256())
	if err != nil {
		return err
	}

	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.Capacity = btcutil.Amount(endian.Uint64(scratch[:]))
	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.OurBalance = btcutil.Amount(endian.Uint64(scratch[:]))
	if _, err := b.Read(scratch[:]); err != nil {
		return err
	}
	o.TheirBalance = btcutil.Amount(endian.Uint64(scratch[:]))

	o.TheirCommitTx = wire.NewMsgTx()
	if err := o.TheirCommitTx.Deserialize(b); err != nil {
		return err
	}
	o.OurCommitTx = wire.NewMsgTx()
	if err := o.OurCommitTx.Deserialize(b); err != nil {
		return err
	}

	o.FundingTx = wire.NewMsgTx()
	if err := o.FundingTx.Deserialize(b); err != nil {
		return err
	}

	if _, err := b.Read(encryptedPriv[:]); err != nil {
		return err
	}
	decryptedPriv, err = addrManager.Decrypt(waddrmgr.CKTPrivate, encryptedPriv[:])
	if err != nil {
		return err
	}
	o.MultiSigKey, _ = btcec.PrivKeyFromBytes(btcec.S256(), decryptedPriv)

	var redeemScript [71]byte
	if _, err := b.Read(redeemScript[:]); err != nil {
		return err
	}
	o.FundingRedeemScript = redeemScript[:]

	if _, err := b.Read(o.TheirCurrentRevocation[:]); err != nil {
		return err
	}

	var addr [34]byte
	if _, err := b.Read(addr[:]); err != nil {
		return err
	}
	o.OurDeliveryAddress, err = btcutil.DecodeAddress(string(addr[:]), ActiveNetParams)
	if err != nil {
		return err
	}

	if _, err := b.Read(addr[:]); err != nil {
		return err
	}
	o.TheirDeliveryAddress, err = btcutil.DecodeAddress(string(addr[:]), ActiveNetParams)
	if err != nil {
		return err
	}

	if err := binary.Read(b, endian, &o.CsvDelay); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.NumUpdates); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.TotalSatoshisSent); err != nil {
		return err
	}
	if err := binary.Read(b, endian, &o.TotalSatoshisReceived); err != nil {
		return err
	}

	var unix int64
	if err := binary.Read(b, endian, &unix); err != nil {
		return err
	}
	o.CreationTime = time.Unix(unix, 0)

	return nil
}
