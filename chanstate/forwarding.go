package chanstate

import (
	"encoding/binary"
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
