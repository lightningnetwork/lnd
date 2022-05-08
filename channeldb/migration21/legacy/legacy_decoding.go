package legacy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
	"github.com/lightningnetwork/lnd/channeldb/migration21/common"
	"github.com/lightningnetwork/lnd/kvdb"
)

func deserializeHtlcs(r io.Reader) ([]common.HTLC, error) {
	var numHtlcs uint16
	if err := ReadElement(r, &numHtlcs); err != nil {
		return nil, err
	}

	var htlcs []common.HTLC
	if numHtlcs == 0 {
		return htlcs, nil
	}

	htlcs = make([]common.HTLC, numHtlcs)
	for i := uint16(0); i < numHtlcs; i++ {
		if err := ReadElements(r,
			&htlcs[i].Signature, &htlcs[i].RHash, &htlcs[i].Amt,
			&htlcs[i].RefundTimeout, &htlcs[i].OutputIndex,
			&htlcs[i].Incoming, &htlcs[i].OnionBlob,
			&htlcs[i].HtlcIndex, &htlcs[i].LogIndex,
		); err != nil {
			return htlcs, err
		}
	}

	return htlcs, nil
}

func DeserializeLogUpdates(r io.Reader) ([]common.LogUpdate, error) {
	var numUpdates uint16
	if err := binary.Read(r, byteOrder, &numUpdates); err != nil {
		return nil, err
	}

	logUpdates := make([]common.LogUpdate, numUpdates)
	for i := 0; i < int(numUpdates); i++ {
		err := ReadElements(r,
			&logUpdates[i].LogIndex, &logUpdates[i].UpdateMsg,
		)
		if err != nil {
			return nil, err
		}
	}

	return logUpdates, nil
}

func deserializeChanCommit(r io.Reader) (common.ChannelCommitment, error) {
	var c common.ChannelCommitment

	err := ReadElements(r,
		&c.CommitHeight, &c.LocalLogIndex, &c.LocalHtlcIndex, &c.RemoteLogIndex,
		&c.RemoteHtlcIndex, &c.LocalBalance, &c.RemoteBalance,
		&c.CommitFee, &c.FeePerKw, &c.CommitTx, &c.CommitSig,
	)
	if err != nil {
		return c, err
	}

	c.Htlcs, err = deserializeHtlcs(r)
	if err != nil {
		return c, err
	}

	return c, nil
}

func DeserializeCommitDiff(r io.Reader) (*common.CommitDiff, error) {
	var (
		d   common.CommitDiff
		err error
	)

	d.Commitment, err = deserializeChanCommit(r)
	if err != nil {
		return nil, err
	}

	d.CommitSig = &lnwire.CommitSig{}
	if err := d.CommitSig.Decode(r, 0); err != nil {
		return nil, err
	}

	d.LogUpdates, err = DeserializeLogUpdates(r)
	if err != nil {
		return nil, err
	}

	var numOpenRefs uint16
	if err := binary.Read(r, byteOrder, &numOpenRefs); err != nil {
		return nil, err
	}

	d.OpenedCircuitKeys = make([]common.CircuitKey, numOpenRefs)
	for i := 0; i < int(numOpenRefs); i++ {
		err := ReadElements(r,
			&d.OpenedCircuitKeys[i].ChanID,
			&d.OpenedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	var numClosedRefs uint16
	if err := binary.Read(r, byteOrder, &numClosedRefs); err != nil {
		return nil, err
	}

	d.ClosedCircuitKeys = make([]common.CircuitKey, numClosedRefs)
	for i := 0; i < int(numClosedRefs); i++ {
		err := ReadElements(r,
			&d.ClosedCircuitKeys[i].ChanID,
			&d.ClosedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	return &d, nil
}

func serializeHtlcs(b io.Writer, htlcs ...common.HTLC) error {
	numHtlcs := uint16(len(htlcs))
	if err := WriteElement(b, numHtlcs); err != nil {
		return err
	}

	for _, htlc := range htlcs {
		if err := WriteElements(b,
			htlc.Signature, htlc.RHash, htlc.Amt, htlc.RefundTimeout,
			htlc.OutputIndex, htlc.Incoming, htlc.OnionBlob,
			htlc.HtlcIndex, htlc.LogIndex,
		); err != nil {
			return err
		}
	}

	return nil
}

func serializeChanCommit(w io.Writer, c *common.ChannelCommitment) error {
	if err := WriteElements(w,
		c.CommitHeight, c.LocalLogIndex, c.LocalHtlcIndex,
		c.RemoteLogIndex, c.RemoteHtlcIndex, c.LocalBalance,
		c.RemoteBalance, c.CommitFee, c.FeePerKw, c.CommitTx,
		c.CommitSig,
	); err != nil {
		return err
	}

	return serializeHtlcs(w, c.Htlcs...)
}

func SerializeLogUpdates(w io.Writer, logUpdates []common.LogUpdate) error {
	numUpdates := uint16(len(logUpdates))
	if err := binary.Write(w, byteOrder, numUpdates); err != nil {
		return err
	}

	for _, diff := range logUpdates {
		err := WriteElements(w, diff.LogIndex, diff.UpdateMsg)
		if err != nil {
			return err
		}
	}

	return nil
}

func SerializeCommitDiff(w io.Writer, diff *common.CommitDiff) error { // nolint: dupl
	if err := serializeChanCommit(w, &diff.Commitment); err != nil {
		return err
	}

	if err := diff.CommitSig.Encode(w, 0); err != nil {
		return err
	}

	if err := SerializeLogUpdates(w, diff.LogUpdates); err != nil {
		return err
	}

	numOpenRefs := uint16(len(diff.OpenedCircuitKeys))
	if err := binary.Write(w, byteOrder, numOpenRefs); err != nil {
		return err
	}

	for _, openRef := range diff.OpenedCircuitKeys {
		err := WriteElements(w, openRef.ChanID, openRef.HtlcID)
		if err != nil {
			return err
		}
	}

	numClosedRefs := uint16(len(diff.ClosedCircuitKeys))
	if err := binary.Write(w, byteOrder, numClosedRefs); err != nil {
		return err
	}

	for _, closedRef := range diff.ClosedCircuitKeys {
		err := WriteElements(w, closedRef.ChanID, closedRef.HtlcID)
		if err != nil {
			return err
		}
	}

	return nil
}

func DeserializeNetworkResult(r io.Reader) (*common.NetworkResult, error) {
	var (
		err error
	)

	n := &common.NetworkResult{}

	n.Msg, err = lnwire.ReadMessage(r, 0)
	if err != nil {
		return nil, err
	}

	if err := ReadElements(r,
		&n.Unencrypted, &n.IsResolution,
	); err != nil {
		return nil, err
	}

	return n, nil
}

func SerializeNetworkResult(w io.Writer, n *common.NetworkResult) error {
	if _, err := lnwire.WriteMessage(w, n.Msg, 0); err != nil {
		return err
	}

	return WriteElements(w, n.Unencrypted, n.IsResolution)
}

func readChanConfig(b io.Reader, c *common.ChannelConfig) error { // nolint: dupl
	return ReadElements(b,
		&c.DustLimit, &c.MaxPendingAmount, &c.ChanReserve,
		&c.MinHTLC, &c.MaxAcceptedHtlcs, &c.CsvDelay,
		&c.MultiSigKey, &c.RevocationBasePoint,
		&c.PaymentBasePoint, &c.DelayBasePoint,
		&c.HtlcBasePoint,
	)
}

func DeserializeCloseChannelSummary(
	r io.Reader) (*common.ChannelCloseSummary, error) { // nolint: dupl

	c := &common.ChannelCloseSummary{}

	err := ReadElements(r,
		&c.ChanPoint, &c.ShortChanID, &c.ChainHash, &c.ClosingTXID,
		&c.CloseHeight, &c.RemotePub, &c.Capacity, &c.SettledBalance,
		&c.TimeLockedBalance, &c.CloseType, &c.IsPending,
	)
	if err != nil {
		return nil, err
	}

	// We'll now check to see if the channel close summary was encoded with
	// any of the additional optional fields.
	var hasNewFields bool
	err = ReadElements(r, &hasNewFields)
	if err != nil {
		return nil, err
	}

	// If fields are not present, we can return.
	if !hasNewFields {
		return c, nil
	}

	// Otherwise read the new fields.
	if err := ReadElements(r, &c.RemoteCurrentRevocation); err != nil {
		return nil, err
	}

	if err := readChanConfig(r, &c.LocalChanConfig); err != nil {
		return nil, err
	}

	// Finally, we'll attempt to read the next unrevoked commitment point
	// for the remote party. If we closed the channel before receiving a
	// funding locked message then this might not be present. A boolean
	// indicating whether the field is present will come first.
	var hasRemoteNextRevocation bool
	err = ReadElements(r, &hasRemoteNextRevocation)
	if err != nil {
		return nil, err
	}

	// If this field was written, read it.
	if hasRemoteNextRevocation {
		err = ReadElements(r, &c.RemoteNextRevocation)
		if err != nil {
			return nil, err
		}
	}

	// Check if we have a channel sync message to read.
	var hasChanSyncMsg bool
	err = ReadElements(r, &hasChanSyncMsg)
	if err == io.EOF {
		return c, nil
	} else if err != nil {
		return nil, err
	}

	// If a chan sync message is present, read it.
	if hasChanSyncMsg {
		// We must pass in reference to a lnwire.Message for the codec
		// to support it.
		msg, err := lnwire.ReadMessage(r, 0)
		if err != nil {
			return nil, err
		}

		chanSync, ok := msg.(*lnwire.ChannelReestablish)
		if !ok {
			return nil, errors.New("unable cast db Message to " +
				"ChannelReestablish")
		}
		c.LastChanSyncMsg = chanSync
	}

	return c, nil
}

func writeChanConfig(b io.Writer, c *common.ChannelConfig) error { // nolint: dupl
	return WriteElements(b,
		c.DustLimit, c.MaxPendingAmount, c.ChanReserve, c.MinHTLC,
		c.MaxAcceptedHtlcs, c.CsvDelay, c.MultiSigKey,
		c.RevocationBasePoint, c.PaymentBasePoint, c.DelayBasePoint,
		c.HtlcBasePoint,
	)
}

func SerializeChannelCloseSummary(w io.Writer, cs *common.ChannelCloseSummary) error {
	err := WriteElements(w,
		cs.ChanPoint, cs.ShortChanID, cs.ChainHash, cs.ClosingTXID,
		cs.CloseHeight, cs.RemotePub, cs.Capacity, cs.SettledBalance,
		cs.TimeLockedBalance, cs.CloseType, cs.IsPending,
	)
	if err != nil {
		return err
	}

	// If this is a close channel summary created before the addition of
	// the new fields, then we can exit here.
	if cs.RemoteCurrentRevocation == nil {
		return WriteElements(w, false)
	}

	// If fields are present, write boolean to indicate this, and continue.
	if err := WriteElements(w, true); err != nil {
		return err
	}

	if err := WriteElements(w, cs.RemoteCurrentRevocation); err != nil {
		return err
	}

	if err := writeChanConfig(w, &cs.LocalChanConfig); err != nil {
		return err
	}

	// The RemoteNextRevocation field is optional, as it's possible for a
	// channel to be closed before we learn of the next unrevoked
	// revocation point for the remote party. Write a boolean indicating
	// whether this field is present or not.
	if err := WriteElements(w, cs.RemoteNextRevocation != nil); err != nil {
		return err
	}

	// Write the field, if present.
	if cs.RemoteNextRevocation != nil {
		if err = WriteElements(w, cs.RemoteNextRevocation); err != nil {
			return err
		}
	}

	// Write whether the channel sync message is present.
	if err := WriteElements(w, cs.LastChanSyncMsg != nil); err != nil {
		return err
	}

	// Write the channel sync message, if present.
	if cs.LastChanSyncMsg != nil {
		_, err = lnwire.WriteMessage(w, cs.LastChanSyncMsg, 0)
		if err != nil {
			return err
		}
	}

	return nil
}

// ErrCorruptedFwdPkg signals that the on-disk structure of the forwarding
// package has potentially been mangled.
var ErrCorruptedFwdPkg = errors.New("fwding package db has been corrupted")

var (
	// fwdPackagesKey is the root-level bucket that all forwarding packages
	// are written. This bucket is further subdivided based on the short
	// channel ID of each channel.
	fwdPackagesKey = []byte("fwd-packages")

	// addBucketKey is the bucket to which all Add log updates are written.
	addBucketKey = []byte("add-updates")

	// failSettleBucketKey is the bucket to which all Settle/Fail log
	// updates are written.
	failSettleBucketKey = []byte("fail-settle-updates")

	// fwdFilterKey is a key used to write the set of Adds that passed
	// validation and are to be forwarded to the switch.
	// NOTE: The presence of this key within a forwarding package indicates
	// that the package has reached FwdStateProcessed.
	fwdFilterKey = []byte("fwd-filter-key")

	// ackFilterKey is a key used to access the PkgFilter indicating which
	// Adds have received a Settle/Fail. This response may come from a
	// number of sources, including: exitHop settle/fails, switch failures,
	// chain arbiter interjections, as well as settle/fails from the
	// next hop in the route.
	ackFilterKey = []byte("ack-filter-key")

	// settleFailFilterKey is a key used to access the PkgFilter indicating
	// which Settles/Fails in have been received and processed by the link
	// that originally received the Add.
	settleFailFilterKey = []byte("settle-fail-filter-key")
)

func makeLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}

// uint16Key writes the provided 16-bit unsigned integer to a 2-byte slice.
func uint16Key(i uint16) []byte {
	key := make([]byte, 2)
	byteOrder.PutUint16(key, i)
	return key
}

// ChannelPackager is used by a channel to manage the lifecycle of its forwarding
// packages. The packager is tied to a particular source channel ID, allowing it
// to create and edit its own packages. Each packager also has the ability to
// remove fail/settle htlcs that correspond to an add contained in one of
// source's packages.
type ChannelPackager struct {
	source lnwire.ShortChannelID
}

// NewChannelPackager creates a new packager for a single channel.
func NewChannelPackager(source lnwire.ShortChannelID) *ChannelPackager {
	return &ChannelPackager{
		source: source,
	}
}

// AddFwdPkg writes a newly locked in forwarding package to disk.
func (*ChannelPackager) AddFwdPkg(tx kvdb.RwTx, fwdPkg *common.FwdPkg) error { // nolint: dupl
	fwdPkgBkt, err := tx.CreateTopLevelBucket(fwdPackagesKey)
	if err != nil {
		return err
	}

	source := makeLogKey(fwdPkg.Source.ToUint64())
	sourceBkt, err := fwdPkgBkt.CreateBucketIfNotExists(source[:])
	if err != nil {
		return err
	}

	heightKey := makeLogKey(fwdPkg.Height)
	heightBkt, err := sourceBkt.CreateBucketIfNotExists(heightKey[:])
	if err != nil {
		return err
	}

	// Write ADD updates we received at this commit height.
	addBkt, err := heightBkt.CreateBucketIfNotExists(addBucketKey)
	if err != nil {
		return err
	}

	// Write SETTLE/FAIL updates we received at this commit height.
	failSettleBkt, err := heightBkt.CreateBucketIfNotExists(failSettleBucketKey)
	if err != nil {
		return err
	}

	for i := range fwdPkg.Adds {
		err = putLogUpdate(addBkt, uint16(i), &fwdPkg.Adds[i])
		if err != nil {
			return err
		}
	}

	// Persist the initialized pkg filter, which will be used to determine
	// when we can remove this forwarding package from disk.
	var ackFilterBuf bytes.Buffer
	if err := fwdPkg.AckFilter.Encode(&ackFilterBuf); err != nil {
		return err
	}

	if err := heightBkt.Put(ackFilterKey, ackFilterBuf.Bytes()); err != nil {
		return err
	}

	for i := range fwdPkg.SettleFails {
		err = putLogUpdate(failSettleBkt, uint16(i), &fwdPkg.SettleFails[i])
		if err != nil {
			return err
		}
	}

	var settleFailFilterBuf bytes.Buffer
	err = fwdPkg.SettleFailFilter.Encode(&settleFailFilterBuf)
	if err != nil {
		return err
	}

	return heightBkt.Put(settleFailFilterKey, settleFailFilterBuf.Bytes())
}

// putLogUpdate writes an htlc to the provided `bkt`, using `index` as the key.
func putLogUpdate(bkt kvdb.RwBucket, idx uint16, htlc *common.LogUpdate) error {
	var b bytes.Buffer
	if err := serializeLogUpdate(&b, htlc); err != nil {
		return err
	}

	return bkt.Put(uint16Key(idx), b.Bytes())
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in a map indexed by the
// remote commitment height at which the updates were locked in.
func (p *ChannelPackager) LoadFwdPkgs(tx kvdb.RTx) ([]*common.FwdPkg, error) {
	return loadChannelFwdPkgs(tx, p.source)
}

// loadChannelFwdPkgs loads all forwarding packages owned by `source`.
func loadChannelFwdPkgs(tx kvdb.RTx, source lnwire.ShortChannelID) ([]*common.FwdPkg, error) { // nolint: dupl
	fwdPkgBkt := tx.ReadBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return nil, nil
	}

	sourceKey := makeLogKey(source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadBucket(sourceKey[:])
	if sourceBkt == nil {
		return nil, nil
	}

	var heights []uint64
	if err := sourceBkt.ForEach(func(k, _ []byte) error {
		if len(k) != 8 {
			return ErrCorruptedFwdPkg
		}

		heights = append(heights, byteOrder.Uint64(k))

		return nil
	}); err != nil {
		return nil, err
	}

	// Load the forwarding package for each retrieved height.
	fwdPkgs := make([]*common.FwdPkg, 0, len(heights))
	for _, height := range heights {
		fwdPkg, err := loadFwdPkg(fwdPkgBkt, source, height)
		if err != nil {
			return nil, err
		}

		fwdPkgs = append(fwdPkgs, fwdPkg)
	}

	return fwdPkgs, nil
}

// loadFwdPkg reads the packager's fwd pkg at a given height, and determines the
// appropriate FwdState.
func loadFwdPkg(fwdPkgBkt kvdb.RBucket, source lnwire.ShortChannelID,
	height uint64) (*common.FwdPkg, error) {

	sourceKey := makeLogKey(source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadBucket(sourceKey[:])
	if sourceBkt == nil {
		return nil, ErrCorruptedFwdPkg
	}

	heightKey := makeLogKey(height)
	heightBkt := sourceBkt.NestedReadBucket(heightKey[:])
	if heightBkt == nil {
		return nil, ErrCorruptedFwdPkg
	}

	// Load ADDs from disk.
	addBkt := heightBkt.NestedReadBucket(addBucketKey)
	if addBkt == nil {
		return nil, ErrCorruptedFwdPkg
	}

	adds, err := loadHtlcs(addBkt)
	if err != nil {
		return nil, err
	}

	// Load ack filter from disk.
	ackFilterBytes := heightBkt.Get(ackFilterKey)
	if ackFilterBytes == nil {
		return nil, ErrCorruptedFwdPkg
	}
	ackFilterReader := bytes.NewReader(ackFilterBytes)

	ackFilter := &common.PkgFilter{}
	if err := ackFilter.Decode(ackFilterReader); err != nil {
		return nil, err
	}

	// Load SETTLE/FAILs from disk.
	failSettleBkt := heightBkt.NestedReadBucket(failSettleBucketKey)
	if failSettleBkt == nil {
		return nil, ErrCorruptedFwdPkg
	}

	failSettles, err := loadHtlcs(failSettleBkt)
	if err != nil {
		return nil, err
	}

	// Load settle fail filter from disk.
	settleFailFilterBytes := heightBkt.Get(settleFailFilterKey)
	if settleFailFilterBytes == nil {
		return nil, ErrCorruptedFwdPkg
	}
	settleFailFilterReader := bytes.NewReader(settleFailFilterBytes)

	settleFailFilter := &common.PkgFilter{}
	if err := settleFailFilter.Decode(settleFailFilterReader); err != nil {
		return nil, err
	}

	// Initialize the fwding package, which always starts in the
	// FwdStateLockedIn. We can determine what state the package was left in
	// by examining constraints on the information loaded from disk.
	fwdPkg := &common.FwdPkg{
		Source:           source,
		State:            common.FwdStateLockedIn,
		Height:           height,
		Adds:             adds,
		AckFilter:        ackFilter,
		SettleFails:      failSettles,
		SettleFailFilter: settleFailFilter,
	}

	// Check to see if we have written the set exported filter adds to
	// disk. If we haven't, processing of this package was never started, or
	// failed during the last attempt.
	fwdFilterBytes := heightBkt.Get(fwdFilterKey)
	if fwdFilterBytes == nil {
		nAdds := uint16(len(adds))
		fwdPkg.FwdFilter = common.NewPkgFilter(nAdds)
		return fwdPkg, nil
	}

	fwdFilterReader := bytes.NewReader(fwdFilterBytes)
	fwdPkg.FwdFilter = &common.PkgFilter{}
	if err := fwdPkg.FwdFilter.Decode(fwdFilterReader); err != nil {
		return nil, err
	}

	// Otherwise, a complete round of processing was completed, and we
	// advance the package to FwdStateProcessed.
	fwdPkg.State = common.FwdStateProcessed

	// If every add, settle, and fail has been fully acknowledged, we can
	// safely set the package's state to FwdStateCompleted, signalling that
	// it can be garbage collected.
	if fwdPkg.AckFilter.IsFull() && fwdPkg.SettleFailFilter.IsFull() {
		fwdPkg.State = common.FwdStateCompleted
	}

	return fwdPkg, nil
}

// loadHtlcs retrieves all serialized htlcs in a bucket, returning
// them in order of the indexes they were written under.
func loadHtlcs(bkt kvdb.RBucket) ([]common.LogUpdate, error) {
	var htlcs []common.LogUpdate
	if err := bkt.ForEach(func(_, v []byte) error {
		htlc, err := deserializeLogUpdate(bytes.NewReader(v))
		if err != nil {
			return err
		}

		htlcs = append(htlcs, *htlc)

		return nil
	}); err != nil {
		return nil, err
	}

	return htlcs, nil
}

// serializeLogUpdate writes a log update to the provided io.Writer.
func serializeLogUpdate(w io.Writer, l *common.LogUpdate) error {
	return WriteElements(w, l.LogIndex, l.UpdateMsg)
}

// deserializeLogUpdate reads a log update from the provided io.Reader.
func deserializeLogUpdate(r io.Reader) (*common.LogUpdate, error) {
	l := &common.LogUpdate{}
	if err := ReadElements(r, &l.LogIndex, &l.UpdateMsg); err != nil {
		return nil, err
	}

	return l, nil
}
