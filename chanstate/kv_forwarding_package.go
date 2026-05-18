package chanstate

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"github.com/lightningnetwork/lnd/kvdb"
	"github.com/lightningnetwork/lnd/lnwire"
)

//nolint:ll
var (
	// ErrCorruptedFwdPkg signals that the on-disk structure of the
	// forwarding package has potentially been mangled.
	ErrCorruptedFwdPkg = errors.New("fwding package db has been corrupted")

	// fwdPackagesKey is the root-level bucket that all forwarding packages
	// are written. This bucket is further subdivided based on the short
	// channel ID of each channel.
	//
	// Bucket hierarchy:
	//
	// fwdPackagesKey(root-bucket)
	//     	|
	//     	|-- <shortChannelID>
	//     	|       |
	//     	|       |-- <height>
	//     	|       |       |-- ackFilterKey: <encoded bytes of PkgFilter>
	//     	|       |       |-- settleFailFilterKey: <encoded bytes of PkgFilter>
	//     	|       |       |-- fwdFilterKey: <encoded bytes of PkgFilter>
	//     	|       |       |
	//     	|       |       |-- addBucketKey
	//     	|       |       |        |-- <index of LogUpdate>: <encoded bytes of LogUpdate>
	//     	|       |       |        |-- <index of LogUpdate>: <encoded bytes of LogUpdate>
	//     	|       |       |        ...
	//     	|       |       |
	//     	|       |       |-- failSettleBucketKey
	//     	|       |                |-- <index of LogUpdate>: <encoded bytes of LogUpdate>
	//     	|       |                |-- <index of LogUpdate>: <encoded bytes of LogUpdate>
	//     	|       |                ...
	//     	|       |
	//     	|       |-- <height>
	//     	|       |       |
	//     	|       ...     ...
	//     	|
	//     	|
	//     	|-- <shortChannelID>
	//     	|       |
	//	|       ...
	// 	...
	//
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

// FwdPackagesBucketKey returns the root-level bucket key that stores
// forwarding packages.
func FwdPackagesBucketKey() []byte {
	return fwdPackagesKey
}

// Encode serializes the AddRef to the given io.Writer.
func (a *AddRef) Encode(w io.Writer) error {
	if err := binary.Write(w, binary.BigEndian, a.Height); err != nil {
		return err
	}

	return binary.Write(w, binary.BigEndian, a.Index)
}

// Decode deserializes the AddRef from the given io.Reader.
func (a *AddRef) Decode(r io.Reader) error {
	if err := binary.Read(r, binary.BigEndian, &a.Height); err != nil {
		return err
	}

	return binary.Read(r, binary.BigEndian, &a.Index)
}

// Size returns number of bytes produced when the PkgFilter is serialized.
func (f *PkgFilter) Size() uint16 {
	// 2 bytes for uint16 `count`, then round up number of bytes required to
	// represent `count` bits.
	return 2 + (f.count+7)/8
}

// Encode writes the filter to the provided io.Writer.
func (f *PkgFilter) Encode(w io.Writer) error {
	if err := binary.Write(w, binary.BigEndian, f.count); err != nil {
		return err
	}

	_, err := w.Write(f.filter)

	return err
}

// Decode reads the filter from the provided io.Reader.
func (f *PkgFilter) Decode(r io.Reader) error {
	if err := binary.Read(r, binary.BigEndian, &f.count); err != nil {
		return err
	}

	f.filter = make([]byte, f.Size()-2)
	_, err := io.ReadFull(r, f.filter)

	return err
}

// SettleFailAcker is a generic interface providing the ability to acknowledge
// settle/fail HTLCs stored in forwarding packages.
type SettleFailAcker interface {
	// AckSettleFails atomically updates the settle-fail filters in *other*
	// channels' forwarding packages.
	AckSettleFails(tx kvdb.RwTx, settleFailRefs ...SettleFailRef) error
}

// GlobalFwdPkgReader is an interface used to retrieve the forwarding packages
// of any active channel.
type GlobalFwdPkgReader interface {
	// LoadChannelFwdPkgs loads all known forwarding packages for the given
	// channel.
	LoadChannelFwdPkgs(tx kvdb.RTx,
		source lnwire.ShortChannelID) ([]*FwdPkg, error)
}

// FwdOperator defines the interfaces for managing forwarding packages that are
// external to a particular channel. This interface is used by the switch to
// read forwarding packages from arbitrary channels, and acknowledge settles and
// fails for locally-sourced payments.
type FwdOperator interface {
	// GlobalFwdPkgReader provides read access to all known forwarding
	// packages
	GlobalFwdPkgReader

	// SettleFailAcker grants the ability to acknowledge settles or fails
	// residing in arbitrary forwarding packages.
	SettleFailAcker
}

// SwitchPackager is a concrete implementation of the FwdOperator interface.
// A SwitchPackager offers the ability to read any forwarding package, and ack
// arbitrary settle and fail HTLCs.
type SwitchPackager struct{}

// NewSwitchPackager instantiates a new SwitchPackager.
func NewSwitchPackager() *SwitchPackager {
	return &SwitchPackager{}
}

// AckSettleFails atomically updates the settle-fail filters in *other*
// channels' forwarding packages, to mark that the switch has received a settle
// or fail residing in the forwarding package of a link.
func (*SwitchPackager) AckSettleFails(tx kvdb.RwTx,
	settleFailRefs ...SettleFailRef) error {

	return ackSettleFails(tx, settleFailRefs)
}

// LoadChannelFwdPkgs loads all forwarding packages for a particular channel.
func (*SwitchPackager) LoadChannelFwdPkgs(tx kvdb.RTx,
	source lnwire.ShortChannelID) ([]*FwdPkg, error) {

	return loadChannelFwdPkgs(tx, source)
}

// FwdPackager supports all operations required to modify fwd packages, such as
// creation, updates, reading, and removal. The interfaces are broken down in
// this way to support future delegation of the subinterfaces.
type FwdPackager interface {
	// AddFwdPkg serializes and writes a FwdPkg for this channel at the
	// remote commitment height included in the forwarding package.
	AddFwdPkg(tx kvdb.RwTx, fwdPkg *FwdPkg) error

	// SetFwdFilter looks up the forwarding package at the remote `height`
	// and sets the `fwdFilter`, marking the Adds for which:
	// 1) We are not the exit node
	// 2) Passed all validation
	// 3) Should be forwarded to the switch immediately after a failure
	SetFwdFilter(tx kvdb.RwTx, height uint64, fwdFilter *PkgFilter) error

	// AckAddHtlcs atomically updates the add filters in this channel's
	// forwarding packages to mark the resolution of an Add that was
	// received from the remote party.
	AckAddHtlcs(tx kvdb.RwTx, addRefs ...AddRef) error

	// SettleFailAcker allows a link to acknowledge settle/fail HTLCs
	// belonging to other channels.
	SettleFailAcker

	// LoadFwdPkgs loads all known forwarding packages owned by this
	// channel.
	LoadFwdPkgs(tx kvdb.RTx) ([]*FwdPkg, error)

	// RemovePkg deletes a forwarding package owned by this channel at
	// the provided remote `height`.
	RemovePkg(tx kvdb.RwTx, height uint64) error

	// Wipe deletes all the forwarding packages owned by this channel.
	Wipe(tx kvdb.RwTx) error
}

// ChannelPackager is used by a channel to manage the lifecycle of its
// forwarding packages. The packager is tied to a particular source channel ID,
// allowing it to create and edit its own packages. Each packager also has the
// ability to
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

func newChannelPackager(channel *OpenChannel) *ChannelPackager {
	return NewChannelPackager(channel.ShortChannelID)
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in map indexed by the
// remote commitment height at which the updates were locked in.
func (s *KVStore) LoadFwdPkgs(channel *OpenChannel) ([]*FwdPkg,
	error) {

	var fwdPkgs []*FwdPkg
	if err := kvdb.View(s.backend, func(tx kvdb.RTx) error {
		var err error
		fwdPkgs, err = newChannelPackager(channel).LoadFwdPkgs(tx)
		return err
	}, func() {
		fwdPkgs = nil
	}); err != nil {
		return nil, err
	}

	return fwdPkgs, nil
}

// AckAddHtlcs updates the AckAddFilter containing any of the provided AddRefs
// indicating that a response to this Add has been committed to the remote
// party. Doing so will prevent these Add HTLCs from being reforwarded
// internally.
func (s *KVStore) AckAddHtlcs(channel *OpenChannel,
	addRefs ...AddRef) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		return newChannelPackager(channel).AckAddHtlcs(tx, addRefs...)
	}, func() {})
}

// AckSettleFails updates the SettleFailFilter containing any of the provided
// SettleFailRefs, indicating that the response has been delivered to the
// incoming link, corresponding to a particular AddRef. Doing so will prevent
// the responses from being retransmitted internally.
func (s *KVStore) AckSettleFails(channel *OpenChannel,
	settleFailRefs ...SettleFailRef) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		return newChannelPackager(channel).AckSettleFails(
			tx, settleFailRefs...,
		)
	}, func() {})
}

// SetFwdFilter atomically sets the forwarding filter for the forwarding package
// identified by `height`.
func (s *KVStore) SetFwdFilter(channel *OpenChannel, height uint64,
	fwdFilter *PkgFilter) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		return newChannelPackager(channel).SetFwdFilter(
			tx, height, fwdFilter,
		)
	}, func() {})
}

// RemoveFwdPkgs atomically removes forwarding packages specified by the remote
// commitment heights. If one of the intermediate RemovePkg calls fails, then
// the later packages won't be removed.
//
// NOTE: This method should only be called on packages marked FwdStateCompleted.
func (s *KVStore) RemoveFwdPkgs(channel *OpenChannel,
	heights ...uint64) error {

	return kvdb.Update(s.backend, func(tx kvdb.RwTx) error {
		packager := newChannelPackager(channel)

		for _, height := range heights {
			err := packager.RemovePkg(tx, height)
			if err != nil {
				return err
			}
		}

		return nil
	}, func() {})
}

// AddFwdPkg writes a newly locked in forwarding package to disk.
func (*ChannelPackager) AddFwdPkg(tx kvdb.RwTx, fwdPkg *FwdPkg) error {
	fwdPkgBkt, err := tx.CreateTopLevelBucket(fwdPackagesKey)
	if err != nil {
		return err
	}

	source := forwardingLogKey(fwdPkg.Source.ToUint64())
	sourceBkt, err := fwdPkgBkt.CreateBucketIfNotExists(source[:])
	if err != nil {
		return err
	}

	heightKey := forwardingLogKey(fwdPkg.Height)
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
	failSettleBkt, err := heightBkt.CreateBucketIfNotExists(
		failSettleBucketKey,
	)
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

	err = heightBkt.Put(ackFilterKey, ackFilterBuf.Bytes())
	if err != nil {
		return err
	}

	for i := range fwdPkg.SettleFails {
		err = putLogUpdate(
			failSettleBkt, uint16(i), &fwdPkg.SettleFails[i],
		)
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
func putLogUpdate(bkt kvdb.RwBucket, idx uint16, htlc *LogUpdate) error {
	var b bytes.Buffer
	if err := serializeLogUpdate(&b, htlc); err != nil {
		return err
	}

	return bkt.Put(uint16Key(idx), b.Bytes())
}

// serializeLogUpdate writes a log update to the provided io.Writer.
func serializeLogUpdate(w io.Writer, l *LogUpdate) error {
	return WriteElements(w, l.LogIndex, l.UpdateMsg)
}

// deserializeLogUpdate reads a log update from the provided io.Reader.
func deserializeLogUpdate(r io.Reader) (*LogUpdate, error) {
	l := &LogUpdate{}
	if err := ReadElements(r, &l.LogIndex, &l.UpdateMsg); err != nil {
		return nil, err
	}

	return l, nil
}

// LoadFwdPkgs scans the forwarding log for any packages that haven't been
// processed, and returns their deserialized log updates in a map indexed by the
// remote commitment height at which the updates were locked in.
func (p *ChannelPackager) LoadFwdPkgs(tx kvdb.RTx) ([]*FwdPkg, error) {
	return loadChannelFwdPkgs(tx, p.source)
}

// loadChannelFwdPkgs loads all forwarding packages owned by `source`.
func loadChannelFwdPkgs(tx kvdb.RTx,
	source lnwire.ShortChannelID) ([]*FwdPkg, error) {

	fwdPkgBkt := tx.ReadBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return nil, nil
	}

	sourceKey := forwardingLogKey(source.ToUint64())
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
	fwdPkgs := make([]*FwdPkg, 0, len(heights))
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
	height uint64) (*FwdPkg, error) {

	sourceKey := forwardingLogKey(source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadBucket(sourceKey[:])
	if sourceBkt == nil {
		return nil, ErrCorruptedFwdPkg
	}

	heightKey := forwardingLogKey(height)
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

	ackFilter := &PkgFilter{}
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

	settleFailFilter := &PkgFilter{}
	if err := settleFailFilter.Decode(settleFailFilterReader); err != nil {
		return nil, err
	}

	// Initialize the fwding package, which always starts in the
	// FwdStateLockedIn. We can determine what state the package was left in
	// by examining constraints on the information loaded from disk.
	fwdPkg := &FwdPkg{
		Source:           source,
		State:            FwdStateLockedIn,
		Height:           height,
		Adds:             adds,
		AckFilter:        ackFilter,
		SettleFails:      failSettles,
		SettleFailFilter: settleFailFilter,
	}

	// Check if the forward filter has been persisted to disk.
	// This indicates whether the Adds in this package have been processed.
	//
	// NOTE: We also expect packages with no Adds (settle/fail only packages
	// or empty packages) to have the fwd filter set to signal that the
	// packages have been processed.
	fwdFilterBytes := heightBkt.Get(fwdFilterKey)

	// Handle packages with Adds that haven't been processed yet.
	if fwdFilterBytes == nil {
		// Create a new forward filter for the unprocessed Adds.
		nAdds := uint16(len(adds))
		fwdPkg.FwdFilter = NewPkgFilter(nAdds)

		return fwdPkg, nil
	}

	// Load the existing forward filter from disk.
	fwdFilterReader := bytes.NewReader(fwdFilterBytes)
	fwdPkg.FwdFilter = &PkgFilter{}
	if err := fwdPkg.FwdFilter.Decode(fwdFilterReader); err != nil {
		return nil, err
	}

	// Mark the package as processed since the forward filter exists.
	fwdPkg.State = FwdStateProcessed

	// If every add, settle, and fail has been fully acknowledged, we can
	// safely set the package's state to FwdStateCompleted, signalling that
	// it can be garbage collected.
	if fwdPkg.AckFilter.IsFull() && fwdPkg.SettleFailFilter.IsFull() {
		fwdPkg.State = FwdStateCompleted
	}

	return fwdPkg, nil
}

// loadHtlcs retrieves all serialized htlcs in a bucket, returning
// them in order of the indexes they were written under.
func loadHtlcs(bkt kvdb.RBucket) ([]LogUpdate, error) {
	var htlcs []LogUpdate
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

// SetFwdFilter writes the set of indexes corresponding to Adds at the
// `height` that are to be forwarded to the switch. Calling this method causes
// the forwarding package at `height` to be in FwdStateProcessed. We write this
// forwarding decision so that we always arrive at the same behavior for HTLCs
// leaving this channel. After a restart, we skip validation of these Adds,
// since they are assumed to have already been validated, and make the switch or
// outgoing link responsible for handling replays.
func (p *ChannelPackager) SetFwdFilter(tx kvdb.RwTx, height uint64,
	fwdFilter *PkgFilter) error {

	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return ErrCorruptedFwdPkg
	}

	source := forwardingLogKey(p.source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadWriteBucket(source[:])
	if sourceBkt == nil {
		return ErrCorruptedFwdPkg
	}

	heightKey := forwardingLogKey(height)
	heightBkt := sourceBkt.NestedReadWriteBucket(heightKey[:])
	if heightBkt == nil {
		return ErrCorruptedFwdPkg
	}

	// If the fwd filter has already been written, we return early to avoid
	// modifying the persistent state.
	forwardedAddsBytes := heightBkt.Get(fwdFilterKey)
	if forwardedAddsBytes != nil {
		return nil
	}

	// Otherwise we serialize and write the provided fwd filter.
	var b bytes.Buffer
	if err := fwdFilter.Encode(&b); err != nil {
		return err
	}

	return heightBkt.Put(fwdFilterKey, b.Bytes())
}

// AckAddHtlcs accepts a list of references to add htlcs, and updates the
// AckAddFilter of those forwarding packages to indicate that a settle or fail
// has been received in response to the add.
func (p *ChannelPackager) AckAddHtlcs(tx kvdb.RwTx, addRefs ...AddRef) error {
	if len(addRefs) == 0 {
		return nil
	}

	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return ErrCorruptedFwdPkg
	}

	sourceKey := forwardingLogKey(p.source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadWriteBucket(sourceKey[:])
	if sourceBkt == nil {
		return ErrCorruptedFwdPkg
	}

	// Organize the forward references such that we just get a single slice
	// of indexes for each unique height.
	heightDiffs := make(map[uint64][]uint16)
	for _, addRef := range addRefs {
		heightDiffs[addRef.Height] = append(
			heightDiffs[addRef.Height],
			addRef.Index,
		)
	}

	// Load each height bucket once and remove all acked htlcs at that
	// height.
	for height, indexes := range heightDiffs {
		err := ackAddHtlcsAtHeight(sourceBkt, height, indexes)
		if err != nil {
			return err
		}
	}

	return nil
}

// ackAddHtlcsAtHeight updates the AddAckFilter of a single forwarding package
// with a list of indexes, writing the resulting filter back in its place.
func ackAddHtlcsAtHeight(sourceBkt kvdb.RwBucket, height uint64,
	indexes []uint16) error {

	heightKey := forwardingLogKey(height)
	heightBkt := sourceBkt.NestedReadWriteBucket(heightKey[:])
	if heightBkt == nil {
		// If the height bucket isn't found, this could be because the
		// forwarding package was already removed. We'll return nil to
		// signal that the operation is successful, as there is nothing
		// to ack.
		return nil
	}

	// Load ack filter from disk.
	ackFilterBytes := heightBkt.Get(ackFilterKey)
	if ackFilterBytes == nil {
		return ErrCorruptedFwdPkg
	}

	ackFilter := &PkgFilter{}
	ackFilterReader := bytes.NewReader(ackFilterBytes)
	if err := ackFilter.Decode(ackFilterReader); err != nil {
		return err
	}

	// Update the ack filter for this height.
	for _, index := range indexes {
		ackFilter.Set(index)
	}

	// Write the resulting filter to disk.
	var ackFilterBuf bytes.Buffer
	if err := ackFilter.Encode(&ackFilterBuf); err != nil {
		return err
	}

	return heightBkt.Put(ackFilterKey, ackFilterBuf.Bytes())
}

// AckSettleFails persistently acknowledges settles or fails from a remote
// forwarding package. This should only be called after the source of the Add
// has locked in the settle/fail, or it becomes otherwise safe to forgo
// retransmitting the settle/fail after a restart.
func (p *ChannelPackager) AckSettleFails(tx kvdb.RwTx,
	settleFailRefs ...SettleFailRef) error {

	return ackSettleFails(tx, settleFailRefs)
}

// ackSettleFails persistently acknowledges a batch of settle fail references.
func ackSettleFails(tx kvdb.RwTx, settleFailRefs []SettleFailRef) error {
	if len(settleFailRefs) == 0 {
		return nil
	}

	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return ErrCorruptedFwdPkg
	}

	// Organize the forward references such that we just get a single slice
	// of indexes for each unique destination-height pair.
	destHeightDiffs := make(map[lnwire.ShortChannelID]map[uint64][]uint16)
	for _, settleFailRef := range settleFailRefs {
		destHeights, ok := destHeightDiffs[settleFailRef.Source]
		if !ok {
			destHeights = make(map[uint64][]uint16)
			destHeightDiffs[settleFailRef.Source] = destHeights
		}

		destHeights[settleFailRef.Height] = append(
			destHeights[settleFailRef.Height],
			settleFailRef.Index,
		)
	}

	// With the references organized by destination and height, we now load
	// each remote bucket, and update the settle fail filter for any
	// settle/fail htlcs.
	for dest, destHeights := range destHeightDiffs {
		destKey := forwardingLogKey(dest.ToUint64())
		destBkt := fwdPkgBkt.NestedReadWriteBucket(destKey[:])
		if destBkt == nil {
			// If the destination bucket is not found, this is
			// likely the result of the destination channel being
			// closed and having it's forwarding packages wiped. We
			// won't treat this as an error, because the response
			// will no longer be retransmitted internally.
			continue
		}

		for height, indexes := range destHeights {
			err := ackSettleFailsAtHeight(destBkt, height, indexes)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// ackSettleFailsAtHeight given a destination bucket, acks the provided indexes
// at particular a height by updating the settle fail filter.
func ackSettleFailsAtHeight(destBkt kvdb.RwBucket, height uint64,
	indexes []uint16) error {

	heightKey := forwardingLogKey(height)
	heightBkt := destBkt.NestedReadWriteBucket(heightKey[:])
	if heightBkt == nil {
		// If the height bucket isn't found, this could be because the
		// forwarding package was already removed. We'll return nil to
		// signal that the operation is as there is nothing to ack.
		return nil
	}

	// Load ack filter from disk.
	settleFailFilterBytes := heightBkt.Get(settleFailFilterKey)
	if settleFailFilterBytes == nil {
		return ErrCorruptedFwdPkg
	}

	settleFailFilter := &PkgFilter{}
	settleFailFilterReader := bytes.NewReader(settleFailFilterBytes)
	if err := settleFailFilter.Decode(settleFailFilterReader); err != nil {
		return err
	}

	// Update the ack filter for this height.
	for _, index := range indexes {
		settleFailFilter.Set(index)
	}

	// Write the resulting filter to disk.
	var settleFailFilterBuf bytes.Buffer
	if err := settleFailFilter.Encode(&settleFailFilterBuf); err != nil {
		return err
	}

	return heightBkt.Put(settleFailFilterKey, settleFailFilterBuf.Bytes())
}

// RemovePkg deletes the forwarding package at the given height from the
// packager's source bucket.
func (p *ChannelPackager) RemovePkg(tx kvdb.RwTx, height uint64) error {
	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return nil
	}

	sourceBytes := forwardingLogKey(p.source.ToUint64())
	sourceBkt := fwdPkgBkt.NestedReadWriteBucket(sourceBytes[:])
	if sourceBkt == nil {
		return ErrCorruptedFwdPkg
	}

	heightKey := forwardingLogKey(height)

	return sourceBkt.DeleteNestedBucket(heightKey[:])
}

// Wipe deletes all the channel's forwarding packages, if any.
func (p *ChannelPackager) Wipe(tx kvdb.RwTx) error {
	// If the root bucket doesn't exist, there's no need to delete.
	fwdPkgBkt := tx.ReadWriteBucket(fwdPackagesKey)
	if fwdPkgBkt == nil {
		return nil
	}

	sourceBytes := forwardingLogKey(p.source.ToUint64())

	// If the nested bucket doesn't exist, there's no need to delete.
	if fwdPkgBkt.NestedReadWriteBucket(sourceBytes[:]) == nil {
		return nil
	}

	return fwdPkgBkt.DeleteNestedBucket(sourceBytes[:])
}

// uint16Key writes the provided 16-bit unsigned integer to a 2-byte slice.
func uint16Key(i uint16) []byte {
	key := make([]byte, 2)
	byteOrder.PutUint16(key, i)
	return key
}

// forwardingLogKey converts a uint64 into an 8 byte forwarding package key.
func forwardingLogKey(updateNum uint64) [8]byte {
	var key [8]byte
	byteOrder.PutUint64(key[:], updateNum)
	return key
}

// Compile-time constraint to ensure that ChannelPackager implements the public
// FwdPackager interface.
var _ FwdPackager = (*ChannelPackager)(nil)

// Compile-time constraint to ensure that SwitchPackager implements the public
// FwdOperator interface.
var _ FwdOperator = (*SwitchPackager)(nil)
