package channeldb

import (
	"bytes"
	"io"
	"math"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/lnwire"
	pmt "github.com/lightningnetwork/lnd/payments"
)

// serializeHTLCSettleInfo serializes the details of a settled htlc.
func serializeHTLCSettleInfo(w io.Writer, s *pmt.HTLCSettleInfo) error {
	if _, err := w.Write(s.Preimage[:]); err != nil {
		return err
	}

	if err := serializeTime(w, s.SettleTime); err != nil {
		return err
	}

	return nil
}

// deserializeHTLCSettleInfo deserializes the details of a settled htlc.
func deserializeHTLCSettleInfo(r io.Reader) (*pmt.HTLCSettleInfo, error) {
	s := &pmt.HTLCSettleInfo{}
	if _, err := io.ReadFull(r, s.Preimage[:]); err != nil {
		return nil, err
	}

	var err error
	s.SettleTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	return s, nil
}

// serializeHTLCFailInfo serializes the details of a failed htlc including the
// wire failure.
func serializeHTLCFailInfo(w io.Writer, f *pmt.HTLCFailInfo) error {
	if err := serializeTime(w, f.FailTime); err != nil {
		return err
	}

	// Write failure. If there is no failure message, write an empty
	// byte slice.
	var messageBytes bytes.Buffer
	if f.Message != nil {
		err := lnwire.EncodeFailureMessage(&messageBytes, f.Message, 0)
		if err != nil {
			return err
		}
	}
	if err := wire.WriteVarBytes(w, 0, messageBytes.Bytes()); err != nil {
		return err
	}

	return WriteElements(w, byte(f.Reason), f.FailureSourceIndex)
}

// deserializeHTLCFailInfo deserializes the details of a failed htlc including
// the wire failure.
func deserializeHTLCFailInfo(r io.Reader) (*pmt.HTLCFailInfo, error) {
	f := &pmt.HTLCFailInfo{}
	var err error
	f.FailTime, err = deserializeTime(r)
	if err != nil {
		return nil, err
	}

	// Read failure.
	failureBytes, err := wire.ReadVarBytes(
		r, 0, math.MaxUint16, "failure",
	)
	if err != nil {
		return nil, err
	}
	if len(failureBytes) > 0 {
		f.Message, err = lnwire.DecodeFailureMessage(
			bytes.NewReader(failureBytes), 0,
		)
		if err != nil {
			return nil, err
		}
	}

	var reason byte
	err = ReadElements(r, &reason, &f.FailureSourceIndex)
	if err != nil {
		return nil, err
	}
	f.Reason = pmt.HTLCFailReason(reason)

	return f, nil
}

// deserializeTime deserializes time as unix nanoseconds.
func deserializeTime(r io.Reader) (time.Time, error) {
	var scratch [8]byte
	if _, err := io.ReadFull(r, scratch[:]); err != nil {
		return time.Time{}, err
	}

	// Convert to time.Time. Interpret unix nano time zero as a zero
	// time.Time value.
	unixNano := byteOrder.Uint64(scratch[:])
	if unixNano == 0 {
		return time.Time{}, nil
	}

	return time.Unix(0, int64(unixNano)), nil
}

// serializeTime serializes time as unix nanoseconds.
func serializeTime(w io.Writer, t time.Time) error {
	var scratch [8]byte

	// Convert to unix nano seconds, but only if time is non-zero. Calling
	// UnixNano() on a zero time yields an undefined result.
	var unixNano int64
	if !t.IsZero() {
		unixNano = t.UnixNano()
	}

	byteOrder.PutUint64(scratch[:], uint64(unixNano))
	_, err := w.Write(scratch[:])
	return err
}
