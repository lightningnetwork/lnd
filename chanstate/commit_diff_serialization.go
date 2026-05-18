package chanstate

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// SerializeCommitDiff serializes the commit diff.
func SerializeCommitDiff(w io.Writer, diff *CommitDiff) error {
	if err := SerializeChanCommit(w, &diff.Commitment); err != nil {
		return err
	}

	if err := WriteElements(w, diff.CommitSig); err != nil {
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

	// We'll also encode the commit aux data stream here. We do this here
	// rather than above (at the call to serializeChanCommit), to ensure
	// backwards compat for reads to existing non-custom channels.
	if err := EncodeCommitTlvData(w, &diff.Commitment); err != nil {
		return fmt.Errorf("unable to write aux data: %w", err)
	}

	return nil
}

// DeserializeCommitDiff deserializes the commit diff.
func DeserializeCommitDiff(r io.Reader) (*CommitDiff, error) {
	var (
		d   CommitDiff
		err error
	)

	d.Commitment, err = DeserializeChanCommit(r)
	if err != nil {
		return nil, err
	}

	var msg lnwire.Message
	if err := ReadElements(r, &msg); err != nil {
		return nil, err
	}
	commitSig, ok := msg.(*lnwire.CommitSig)
	if !ok {
		return nil, fmt.Errorf("expected lnwire.CommitSig, instead "+
			"read: %T", msg)
	}
	d.CommitSig = commitSig

	d.LogUpdates, err = DeserializeLogUpdates(r)
	if err != nil {
		return nil, err
	}

	var numOpenRefs uint16
	if err := binary.Read(r, byteOrder, &numOpenRefs); err != nil {
		return nil, err
	}

	d.OpenedCircuitKeys = make([]models.CircuitKey, numOpenRefs)
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

	d.ClosedCircuitKeys = make([]models.CircuitKey, numClosedRefs)
	for i := 0; i < int(numClosedRefs); i++ {
		err := ReadElements(r,
			&d.ClosedCircuitKeys[i].ChanID,
			&d.ClosedCircuitKeys[i].HtlcID)
		if err != nil {
			return nil, err
		}
	}

	// As a final step, we'll read out any aux commit data that we have at
	// the end of this byte stream. We do this here to ensure backward
	// compatibility, as otherwise we risk erroneously reading into the
	// wrong field.
	if err := DecodeCommitTlvData(r, &d.Commitment); err != nil {
		return nil, fmt.Errorf("unable to decode aux data: %w", err)
	}

	return &d, nil
}
