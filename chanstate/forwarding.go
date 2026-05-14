package chanstate

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/lnwire"
)

// AddRef is used to identify a particular Add in a FwdPkg. The short channel ID
// is assumed to be that of the packager.
type AddRef struct {
	// Height is the remote commitment height that locked in the Add.
	Height uint64

	// Index is the index of the Add within the fwd pkg's Adds.
	//
	// NOTE: This index is static over the lifetime of a forwarding package.
	Index uint16
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

// SettleFailRef is used to locate a Settle/Fail in another channel's FwdPkg. A
// channel does not remove its own Settle/Fail htlcs, so the source is provided
// to locate a db bucket belonging to another channel.
type SettleFailRef struct {
	// Source identifies the outgoing link that locked in the settle or
	// fail. This is then used by the *incoming* link to find the settle
	// fail in another link's forwarding packages.
	Source lnwire.ShortChannelID

	// Height is the remote commitment height that locked in this
	// Settle/Fail.
	Height uint64

	// Index is the index of the Add with the fwd pkg's SettleFails.
	//
	// NOTE: This index is static over the lifetime of a forwarding package.
	Index uint16
}

// FwdState is an enum used to describe the lifecycle of a FwdPkg.
type FwdState byte

const (
	// FwdStateLockedIn is the starting state for all forwarding packages.
	// Packages in this state have not yet committed to the exact set of
	// Adds to forward to the switch.
	FwdStateLockedIn FwdState = iota

	// FwdStateProcessed marks the state in which all Adds have been
	// locally processed and the forwarding decision to the switch has been
	// persisted.
	FwdStateProcessed

	// FwdStateCompleted signals that all Adds have been acked, and that all
	// settles and fails have been delivered to their sources. Packages in
	// this state can be removed permanently.
	FwdStateCompleted
)

// PkgFilter is used to compactly represent a particular subset of the Adds in a
// forwarding package. Each filter is represented as a simple, statically-sized
// bitvector, where the elements are intended to be the indices of the Adds as
// they are written in the FwdPkg.
type PkgFilter struct {
	count  uint16
	filter []byte
}

// NewPkgFilter initializes an empty PkgFilter supporting `count` elements.
func NewPkgFilter(count uint16) *PkgFilter {
	// We add 7 to ensure that the integer division yields properly rounded
	// values.
	filterLen := (count + 7) / 8

	return &PkgFilter{
		count:  count,
		filter: make([]byte, filterLen),
	}
}

// Count returns the number of elements represented by this PkgFilter.
func (f *PkgFilter) Count() uint16 {
	return f.count
}

// Set marks the `i`-th element as included by this filter.
// NOTE: It is assumed that i is always less than count.
func (f *PkgFilter) Set(i uint16) {
	byt := i / 8
	bit := i % 8

	// Set the i-th bit in the filter.
	// TODO(conner): ignore if > count to prevent panic?
	f.filter[byt] |= byte(1 << (7 - bit))
}

// Contains queries the filter for membership of index `i`.
// NOTE: It is assumed that i is always less than count.
func (f *PkgFilter) Contains(i uint16) bool {
	byt := i / 8
	bit := i % 8

	// Read the i-th bit in the filter.
	// TODO(conner): ignore if > count to prevent panic?
	return f.filter[byt]&(1<<(7-bit)) != 0
}

// Equal checks two PkgFilters for equality.
func (f *PkgFilter) Equal(f2 *PkgFilter) bool {
	if f == f2 {
		return true
	}
	if f.count != f2.count {
		return false
	}

	return bytes.Equal(f.filter, f2.filter)
}

// IsFull returns true if every element in the filter has been Set, and false
// otherwise.
func (f *PkgFilter) IsFull() bool {
	// Batch validate bytes that are fully used.
	for i := uint16(0); i < f.count/8; i++ {
		if f.filter[i] != 0xFF {
			return false
		}
	}

	// If the count is not a multiple of 8, check that the filter contains
	// all remaining bits.
	rem := f.count % 8
	for idx := f.count - rem; idx < f.count; idx++ {
		if !f.Contains(idx) {
			return false
		}
	}

	return true
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

// String returns a human-readable string.
func (f *PkgFilter) String() string {
	return fmt.Sprintf("count=%v, filter=%v", f.count, f.filter)
}

// FwdPkg records all adds, settles, and fails that were locked in as a result
// of the remote peer sending us a revocation. Each package is identified by
// the short chanid and remote commitment height corresponding to the revocation
// that locked in the HTLCs. For everything except a locally initiated payment,
// settles and fails in a forwarding package must have a corresponding Add in
// another package, and can be removed individually once the source link has
// received the fail/settle.
//
// Adds cannot be removed, as we need to present the same batch of Adds to
// properly handle replay protection. Instead, we use a PkgFilter to mark that
// we have finished processing a particular Add. A FwdPkg should only be deleted
// after the AckFilter is full and all settles and fails have been persistently
// removed.
type FwdPkg struct {
	// Source identifies the channel that wrote this forwarding package.
	Source lnwire.ShortChannelID

	// Height is the height of the remote commitment chain that locked in
	// this forwarding package.
	Height uint64

	// State signals the persistent condition of the package and directs how
	// to reprocess the package in the event of failures.
	State FwdState

	// Adds contains all add messages which need to be processed and
	// forwarded to the switch. Adds does not change over the life of a
	// forwarding package.
	Adds []LogUpdate

	// FwdFilter is a filter containing the indices of all Adds that were
	// forwarded to the switch.
	//
	// NOTE: This value signals when persisted to disk that the fwd package
	// has been processed and garbage collection can happen. So it also
	// has to be set for packages with no adds (empty packages or only
	// settle/fail packages) so that they can be garbage collected as well.
	FwdFilter *PkgFilter

	// AckFilter is a filter containing the indices of all Adds for which
	// the source has received a settle or fail and is reflected in the next
	// commitment txn. A package should not be removed until IsFull()
	// returns true.
	AckFilter *PkgFilter

	// SettleFails contains all settle and fail messages that should be
	// forwarded to the switch.
	SettleFails []LogUpdate

	// SettleFailFilter is a filter containing the indices of all Settle or
	// Fails originating in this package that have been received and locked
	// into the incoming link's commitment state.
	SettleFailFilter *PkgFilter
}

// NewFwdPkg initializes a new forwarding package in FwdStateLockedIn. This
// should be used to create a package at the time we receive a revocation.
func NewFwdPkg(source lnwire.ShortChannelID, height uint64,
	addUpdates, settleFailUpdates []LogUpdate) *FwdPkg {

	nAddUpdates := uint16(len(addUpdates))
	nSettleFailUpdates := uint16(len(settleFailUpdates))

	return &FwdPkg{
		Source:           source,
		Height:           height,
		State:            FwdStateLockedIn,
		Adds:             addUpdates,
		FwdFilter:        NewPkgFilter(nAddUpdates),
		AckFilter:        NewPkgFilter(nAddUpdates),
		SettleFails:      settleFailUpdates,
		SettleFailFilter: NewPkgFilter(nSettleFailUpdates),
	}
}

// SourceRef is a convenience method that returns an AddRef to this forwarding
// package for the index in the argument. It is the caller's responsibility
// to ensure that the index is in bounds.
func (f *FwdPkg) SourceRef(i uint16) AddRef {
	return AddRef{
		Height: f.Height,
		Index:  i,
	}
}

// DestRef is a convenience method that returns a SettleFailRef to this
// forwarding package for the index in the argument. It is the caller's
// responsibility to ensure that the index is in bounds.
func (f *FwdPkg) DestRef(i uint16) SettleFailRef {
	return SettleFailRef{
		Source: f.Source,
		Height: f.Height,
		Index:  i,
	}
}

// ID returns an unique identifier for this package, used to ensure that sphinx
// replay processing of this batch is idempotent.
func (f *FwdPkg) ID() []byte {
	var id = make([]byte, 16)
	binary.BigEndian.PutUint64(id[:8], f.Source.ToUint64())
	binary.BigEndian.PutUint64(id[8:], f.Height)
	return id
}

// String returns a human-readable description of the forwarding package.
func (f *FwdPkg) String() string {
	return fmt.Sprintf("%T(src=%v, height=%v, nadds=%v, nfailsettles=%v)",
		f, f.Source, f.Height, len(f.Adds), len(f.SettleFails))
}
