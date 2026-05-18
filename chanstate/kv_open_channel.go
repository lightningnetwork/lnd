package chanstate

import (
	"bytes"
	"fmt"
	"io"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/lightningnetwork/lnd/fn/v2"
	graphdb "github.com/lightningnetwork/lnd/graph/db"
	"github.com/lightningnetwork/lnd/keychain"
	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

var (
	// dataLossCommitPointKey stores the commitment point received from the
	// remote peer during a channel sync in case we have lost channel state.
	dataLossCommitPointKey = []byte("data-loss-commit-point-key")
)

// DataLossCommitPointKey returns the key used to store the data-loss commit
// point in a channel bucket.
func DataLossCommitPointKey() []byte {
	return dataLossCommitPointKey
}

// PutChannelDataLossCommitPoint stores the data-loss commit point in the
// target channel bucket.
func PutChannelDataLossCommitPoint(chanBucket kvdb.RwBucket,
	commitPoint *btcec.PublicKey) error {

	return chanBucket.Put(
		dataLossCommitPointKey, commitPoint.SerializeCompressed(),
	)
}

// FetchChannelDataLossCommitPoint retrieves the data-loss commit point from the
// target channel bucket.
func FetchChannelDataLossCommitPoint(
	chanBucket kvdb.RBucket) (*btcec.PublicKey, error) {

	bs := chanBucket.Get(dataLossCommitPointKey)
	if bs == nil {
		return nil, ErrNoCommitPoint
	}

	var b [btcec.PubKeyBytesLenCompressed]byte
	r := bytes.NewReader(bs)
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(b[:])
}

const (
	// A tlv type definition used to serialize an outpoint's indexStatus
	// for use in the outpoint index.
	indexStatusType tlv.Type = 0

	// IndexStatusType is the TLV type used to serialize an outpoint's
	// indexStatus for use in the outpoint index.
	IndexStatusType = indexStatusType
)

// indexStatus is an enum-like type that describes what state the outpoint is
// in. Currently only two possible values.
type indexStatus uint8

// IndexStatus is an enum-like type that describes what state the outpoint is
// in. Currently only two possible values.
type IndexStatus = indexStatus

const (
	// outpointOpen represents an outpoint that is open in the outpoint
	// index.
	outpointOpen indexStatus = 0

	// OutpointOpen represents an outpoint that is open in the outpoint
	// index.
	OutpointOpen = outpointOpen

	// outpointClosed represents an outpoint that is closed in the outpoint
	// index.
	outpointClosed indexStatus = 1

	// OutpointClosed represents an outpoint that is closed in the outpoint
	// index.
	OutpointClosed = outpointClosed
)

func putOutpointIndexStatus(opBucket kvdb.RwBucket, chanKey []byte,
	status indexStatus) error {

	statusByte := uint8(status)
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &statusByte)
	opStream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return err
	}

	var b bytes.Buffer
	if err := opStream.Encode(&b); err != nil {
		return err
	}

	return opBucket.Put(chanKey, b.Bytes())
}

// PutOpenOutpointIndex stores chanKey in the outpoint index as an open
// outpoint.
func PutOpenOutpointIndex(opBucket kvdb.RwBucket, chanKey []byte) error {
	return putOutpointIndexStatus(opBucket, chanKey, outpointOpen)
}

// UpdateClosedOutpointIndex flips the outpoint index entry for chanKey from
// open to closed. The index entry must already exist; it was placed there when
// the channel was opened.
func UpdateClosedOutpointIndex(tx kvdb.RwTx, chanKey []byte) error {
	opBucket := tx.ReadWriteBucket(outpointBucket)
	if opBucket == nil {
		return ErrNoChanDBExists
	}
	if opBucket.Get(chanKey) == nil {
		return ErrMissingIndexEntry
	}

	return putOutpointIndexStatus(opBucket, chanKey, outpointClosed)
}

// IsOutpointClosed reports whether the supplied chanKey has been flipped to
// outpointClosed in the supplied outpointBucket. The flip is performed in the
// same transaction as the rest of CloseChannel (sync and tombstone paths
// alike), so a true result is the authoritative "this channel went through
// CloseChannel" signal. On tombstone-enabled backends the chanBucket may still
// exist on disk; readers consult this helper to skip those entries. Callers
// fetch outpointBucket once and pass it in, which lets loop-style readers
// hoist the bucket lookup out of the inner loop.
func IsOutpointClosed(opBucket kvdb.RBucket, chanKey []byte) (bool, error) {
	if opBucket == nil {
		return false, nil
	}
	raw := opBucket.Get(chanKey)
	if raw == nil {
		return false, nil
	}

	var status uint8
	statusRecord := tlv.MakePrimitiveRecord(indexStatusType, &status)
	stream, err := tlv.NewStream(statusRecord)
	if err != nil {
		return false, err
	}
	if err := stream.Decode(bytes.NewReader(raw)); err != nil {
		return false, fmt.Errorf("decode outpoint status for "+
			"chan_key=%x: %w", chanKey, err)
	}

	return indexStatus(status) == outpointClosed, nil
}

// FetchChanBucket is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on.
func FetchChanBucket(tx kvdb.RTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (
	kvdb.RBucket, error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error.

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := IsOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

// FetchChanBucketRw is a helper function that returns the bucket where a
// channel's data resides in given: the public key for the node, the outpoint,
// and the chainhash that the channel resides on. This differs from
// FetchChanBucket in that it returns a writeable bucket.
func FetchChanBucketRw(tx kvdb.RwTx, nodeKey *btcec.PublicKey,
	outPoint *wire.OutPoint, chainHash chainhash.Hash) (kvdb.RwBucket,
	error) {

	// First fetch the top level bucket which stores all data related to
	// current, active channels.
	openChanBucket := tx.ReadWriteBucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, ErrNoChanDBExists
	}

	// TODO(roasbeef): CreateTopLevelBucket on the interface isn't like
	// CreateIfNotExists, will return error.

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	nodePub := nodeKey.SerializeCompressed()
	nodeChanBucket := openChanBucket.NestedReadWriteBucket(nodePub)
	if nodeChanBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// We'll then recurse down an additional layer in order to fetch the
	// bucket for this particular chain.
	chainBucket := nodeChanBucket.NestedReadWriteBucket(chainHash[:])
	if chainBucket == nil {
		return nil, ErrNoActiveChannels
	}

	// With the bucket for the node and chain fetched, we can now go down
	// another level, for this channel itself.
	var chanPointBuf bytes.Buffer
	if err := graphdb.WriteOutpoint(&chanPointBuf, outPoint); err != nil {
		return nil, err
	}
	chanKey := chanPointBuf.Bytes()

	// Treat already-closed channels as gone. The chanBucket may still
	// exist on tombstone-enabled backends; the outpoint flip is the
	// source of truth.
	closed, err := IsOutpointClosed(tx.ReadBucket(outpointBucket), chanKey)
	if err != nil {
		return nil, err
	}
	if closed {
		return nil, ErrChannelNotFound
	}

	chanBucket := chainBucket.NestedReadWriteBucket(chanKey)
	if chanBucket == nil {
		return nil, ErrChannelNotFound
	}

	return chanBucket, nil
}

// keyLocRecord is a wrapper struct around keychain.KeyLocator to implement the
// tlv.RecordProducer interface.
type keyLocRecord struct {
	keychain.KeyLocator
}

// Record creates a Record out of a KeyLocator using the passed Type and the
// EKeyLocator and DKeyLocator functions. The size will always be 8 as
// KeyFamily is uint32 and the Index is uint32.
//
// NOTE: This is part of the tlv.RecordProducer interface.
func (k *keyLocRecord) Record() tlv.Record {
	// Note that we set the type here as zero, as when used with a
	// tlv.RecordT, the type param will be used as the type.
	return tlv.MakeStaticRecord(
		0, &k.KeyLocator, 8, EKeyLocator, DKeyLocator,
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

// openChannelTlvData houses the new data fields that are stored for each
// channel in a TLV stream within the root bucket. This is stored as a TLV
// stream appended to the existing hard-coded fields in the channel's root
// bucket. New fields being added to the channel state should be added here.
//
// NOTE: This struct is used for serialization purposes only and its fields
// should be accessed via the OpenChannel struct while in memory.
type openChannelTlvData struct {
	// revokeKeyLoc is the key locator for the revocation key.
	revokeKeyLoc tlv.RecordT[tlv.TlvType1, keyLocRecord]

	// initialLocalBalance is the initial local balance of the channel.
	initialLocalBalance tlv.RecordT[tlv.TlvType2, uint64]

	// initialRemoteBalance is the initial remote balance of the channel.
	initialRemoteBalance tlv.RecordT[tlv.TlvType3, uint64]

	// realScid is the real short channel ID of the channel corresponding to
	// the on-chain outpoint.
	realScid tlv.RecordT[tlv.TlvType4, lnwire.ShortChannelID]

	// memo is an optional text field that gives context to the user about
	// the channel.
	memo tlv.OptionalRecordT[tlv.TlvType5, []byte]

	// tapscriptRoot is the optional Tapscript root the channel funding
	// output commits to.
	tapscriptRoot tlv.OptionalRecordT[tlv.TlvType6, [32]byte]

	// customBlob is an optional TLV encoded blob of data representing
	// custom channel funding information.
	customBlob tlv.OptionalRecordT[tlv.TlvType7, tlv.Blob]

	// confirmationHeight records the block height at which the funding
	// transaction was first confirmed.
	confirmationHeight tlv.RecordT[tlv.TlvType8, uint32]

	// closeConfirmationHeight records the block height at which the closing
	// transaction was first confirmed. This is used to calculate the
	// remaining confirmations until the channel is considered fully closed.
	// Note: if not set, it means either the channel has not been
	// closed yet, or it was closed before this field was introduced.
	closeConfirmationHeight tlv.OptionalRecordT[tlv.TlvType9, uint32]
}

// encode serializes the openChannelTlvData to the given io.Writer.
func (c *openChannelTlvData) encode(w io.Writer) error {
	tlvRecords := []tlv.Record{
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		c.confirmationHeight.Record(),
	}
	c.memo.WhenSome(func(memo tlv.RecordT[tlv.TlvType5, []byte]) {
		tlvRecords = append(tlvRecords, memo.Record())
	})
	c.tapscriptRoot.WhenSome(
		func(root tlv.RecordT[tlv.TlvType6, [32]byte]) {
			tlvRecords = append(tlvRecords, root.Record())
		},
	)
	c.customBlob.WhenSome(func(blob tlv.RecordT[tlv.TlvType7, tlv.Blob]) {
		tlvRecords = append(tlvRecords, blob.Record())
	})
	c.closeConfirmationHeight.WhenSome(
		func(h tlv.RecordT[tlv.TlvType9, uint32]) {
			tlvRecords = append(tlvRecords, h.Record())
		},
	)

	tlv.SortRecords(tlvRecords)

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(tlvRecords...)
	if err != nil {
		return err
	}

	return tlvStream.Encode(w)
}

// decode deserializes the openChannelTlvData from the given io.Reader.
func (c *openChannelTlvData) decode(r io.Reader) error {
	memo := c.memo.Zero()
	tapscriptRoot := c.tapscriptRoot.Zero()
	blob := c.customBlob.Zero()
	closeConfHeight := c.closeConfirmationHeight.Zero()

	// Create the tlv stream.
	tlvStream, err := tlv.NewStream(
		c.revokeKeyLoc.Record(),
		c.initialLocalBalance.Record(),
		c.initialRemoteBalance.Record(),
		c.realScid.Record(),
		memo.Record(),
		tapscriptRoot.Record(),
		blob.Record(),
		c.confirmationHeight.Record(),
		closeConfHeight.Record(),
	)
	if err != nil {
		return err
	}

	tlvs, err := tlvStream.DecodeWithParsedTypes(r)
	if err != nil {
		return err
	}

	if _, ok := tlvs[memo.TlvType()]; ok {
		c.memo = tlv.SomeRecordT(memo)
	}
	if _, ok := tlvs[tapscriptRoot.TlvType()]; ok {
		c.tapscriptRoot = tlv.SomeRecordT(tapscriptRoot)
	}
	if _, ok := tlvs[c.customBlob.TlvType()]; ok {
		c.customBlob = tlv.SomeRecordT(blob)
	}
	if _, ok := tlvs[closeConfHeight.TlvType()]; ok {
		c.closeConfirmationHeight = tlv.SomeRecordT(closeConfHeight)
	}

	return nil
}

// DecodeOpenChannelTlvData decodes and applies auxiliary TLV data to an open
// channel.
func DecodeOpenChannelTlvData(r io.Reader, channel *OpenChannel) error {
	var auxData openChannelTlvData
	if err := auxData.decode(r); err != nil {
		return err
	}

	amendOpenChannelTlvData(channel, auxData)

	return nil
}

// EncodeOpenChannelTlvData extracts and encodes auxiliary TLV data from an open
// channel.
func EncodeOpenChannelTlvData(w io.Writer, channel *OpenChannel) error {
	auxData := extractOpenChannelTlvData(channel)
	return auxData.encode(w)
}

// amendOpenChannelTlvData updates the channel with the given auxiliary TLV
// data.
func amendOpenChannelTlvData(channel *OpenChannel, auxData openChannelTlvData) {
	channel.RevocationKeyLocator = auxData.revokeKeyLoc.Val.KeyLocator
	channel.InitialLocalBalance = lnwire.MilliSatoshi(
		auxData.initialLocalBalance.Val,
	)
	channel.InitialRemoteBalance = lnwire.MilliSatoshi(
		auxData.initialRemoteBalance.Val,
	)
	channel.SetConfirmedScidForStore(auxData.realScid.Val)
	channel.ConfirmationHeight = auxData.confirmationHeight.Val

	auxData.memo.WhenSomeV(func(memo []byte) {
		channel.Memo = memo
	})
	auxData.tapscriptRoot.WhenSomeV(func(h [32]byte) {
		channel.TapscriptRoot = fn.Some[chainhash.Hash](h)
	})
	auxData.customBlob.WhenSomeV(func(blob tlv.Blob) {
		channel.CustomBlob = fn.Some(blob)
	})
	auxData.closeConfirmationHeight.WhenSomeV(func(h uint32) {
		channel.CloseConfirmationHeight = fn.Some(h)
	})
}

// extractOpenChannelTlvData creates a new openChannelTlvData from the given
// channel.
func extractOpenChannelTlvData(channel *OpenChannel) openChannelTlvData {
	auxData := openChannelTlvData{
		revokeKeyLoc: tlv.NewRecordT[tlv.TlvType1](
			keyLocRecord{channel.RevocationKeyLocator},
		),
		initialLocalBalance: tlv.NewPrimitiveRecord[tlv.TlvType2](
			uint64(channel.InitialLocalBalance),
		),
		initialRemoteBalance: tlv.NewPrimitiveRecord[tlv.TlvType3](
			uint64(channel.InitialRemoteBalance),
		),
		realScid: tlv.NewRecordT[tlv.TlvType4](
			channel.ConfirmedScidForStore(),
		),
		confirmationHeight: tlv.NewPrimitiveRecord[tlv.TlvType8](
			channel.ConfirmationHeight,
		),
	}

	if len(channel.Memo) != 0 {
		auxData.memo = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType5](channel.Memo),
		)
	}
	channel.TapscriptRoot.WhenSome(func(h chainhash.Hash) {
		auxData.tapscriptRoot = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType6, [32]byte](h),
		)
	})
	channel.CustomBlob.WhenSome(func(blob tlv.Blob) {
		auxData.customBlob = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType7](blob),
		)
	})
	channel.CloseConfirmationHeight.WhenSome(func(h uint32) {
		auxData.closeConfirmationHeight = tlv.SomeRecordT(
			tlv.NewPrimitiveRecord[tlv.TlvType9](h),
		)
	})

	return auxData
}
