package tlv

import (
	"encoding/binary"
	"errors"
	"io"
)

// ErrTUintNotMinimal signals that decoding a truncated uint failed because the
// value was not minimally encoded.
var ErrTUintNotMinimal = errors.New("truncated uint not minimally encoded")

// numLeadingZeroBytes16 computes the number of leading zeros for a uint16.
func numLeadingZeroBytes16(v uint16) uint64 {
	switch {
	case v == 0:
		return 2
	case v&0xff00 == 0:
		return 1
	default:
		return 0
	}
}

// SizeTUint16 returns the number of bytes remaining in a uint16 after
// truncating the leading zeros.
func SizeTUint16(v uint16) uint64 {
	return 2 - numLeadingZeroBytes16(v)
}

// ETUint16 is an Encoder for truncated uint16 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint16.
func ETUint16(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*uint16); ok {
		return ETUint16T(w, *t, buf)
	}
	return NewTypeForEncodingErr(val, "uint16")
}

// ETUint16T is an Encoder for truncated uint16 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint16.
func ETUint16T(w io.Writer, val uint16, buf *[8]byte) error {
	binary.BigEndian.PutUint16(buf[:2], val)
	numZeros := numLeadingZeroBytes16(val)
	_, err := w.Write(buf[numZeros:2])
	return err
}

// DTUint16 is an Decoder for truncated uint16 values, where leading zeros will
// be resurrected. An error is returned if val is not a *uint16.
func DTUint16(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if t, ok := val.(*uint16); ok && l <= 2 {
		_, err := io.ReadFull(r, buf[2-l:2])
		if err != nil {
			return err
		}
		zero(buf[:2-l])
		*t = binary.BigEndian.Uint16(buf[:2])
		if 2-numLeadingZeroBytes16(*t) != l {
			return ErrTUintNotMinimal
		}
		return nil
	}
	return NewTypeForDecodingErr(val, "uint16", l, 2)
}

// numLeadingZeroBytes32 computes the number of leading zeros for a uint32.
func numLeadingZeroBytes32(v uint32) uint64 {
	switch {
	case v == 0:
		return 4
	case v&0xffffff00 == 0:
		return 3
	case v&0xffff0000 == 0:
		return 2
	case v&0xff000000 == 0:
		return 1
	default:
		return 0
	}
}

// SizeTUint32 returns the number of bytes remaining in a uint32 after
// truncating the leading zeros.
func SizeTUint32(v uint32) uint64 {
	return 4 - numLeadingZeroBytes32(v)
}

// ETUint32 is an Encoder for truncated uint32 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint32.
func ETUint32(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*uint32); ok {
		return ETUint32T(w, *t, buf)
	}
	return NewTypeForEncodingErr(val, "uint32")
}

// ETUint32T is an Encoder for truncated uint32 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint32.
func ETUint32T(w io.Writer, val uint32, buf *[8]byte) error {
	binary.BigEndian.PutUint32(buf[:4], val)
	numZeros := numLeadingZeroBytes32(val)
	_, err := w.Write(buf[numZeros:4])
	return err
}

// DTUint32 is an Decoder for truncated uint32 values, where leading zeros will
// be resurrected. An error is returned if val is not a *uint32.
func DTUint32(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if t, ok := val.(*uint32); ok && l <= 4 {
		_, err := io.ReadFull(r, buf[4-l:4])
		if err != nil {
			return err
		}
		zero(buf[:4-l])
		*t = binary.BigEndian.Uint32(buf[:4])
		if 4-numLeadingZeroBytes32(*t) != l {
			return ErrTUintNotMinimal
		}
		return nil
	}
	return NewTypeForDecodingErr(val, "uint32", l, 4)
}

// numLeadingZeroBytes64 computes the number of leading zeros for a uint64.
//
// TODO(conner): optimize using unrolled binary search
func numLeadingZeroBytes64(v uint64) uint64 {
	switch {
	case v == 0:
		return 8
	case v&0xffffffffffffff00 == 0:
		return 7
	case v&0xffffffffffff0000 == 0:
		return 6
	case v&0xffffffffff000000 == 0:
		return 5
	case v&0xffffffff00000000 == 0:
		return 4
	case v&0xffffff0000000000 == 0:
		return 3
	case v&0xffff000000000000 == 0:
		return 2
	case v&0xff00000000000000 == 0:
		return 1
	default:
		return 0
	}
}

// SizeTUint64 returns the number of bytes remaining in a uint64 after
// truncating the leading zeros.
func SizeTUint64(v uint64) uint64 {
	return 8 - numLeadingZeroBytes64(v)
}

// ETUint64 is an Encoder for truncated uint64 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint64.
func ETUint64(w io.Writer, val interface{}, buf *[8]byte) error {
	if t, ok := val.(*uint64); ok {
		return ETUint64T(w, *t, buf)
	}
	return NewTypeForEncodingErr(val, "uint64")
}

// ETUint64T is an Encoder for truncated uint64 values, where leading zeros will
// be omitted. An error is returned if val is not a *uint64.
func ETUint64T(w io.Writer, val uint64, buf *[8]byte) error {
	binary.BigEndian.PutUint64(buf[:], val)
	numZeros := numLeadingZeroBytes64(val)
	_, err := w.Write(buf[numZeros:])
	return err
}

// DTUint64 is an Decoder for truncated uint64 values, where leading zeros will
// be resurrected. An error is returned if val is not a *uint64.
func DTUint64(r io.Reader, val interface{}, buf *[8]byte, l uint64) error {
	if t, ok := val.(*uint64); ok && l <= 8 {
		_, err := io.ReadFull(r, buf[8-l:])
		if err != nil {
			return err
		}
		zero(buf[:8-l])
		*t = binary.BigEndian.Uint64(buf[:])
		if 8-numLeadingZeroBytes64(*t) != l {
			return ErrTUintNotMinimal
		}
		return nil
	}
	return NewTypeForDecodingErr(val, "uint64", l, 8)
}

// zero clears the passed byte slice.
func zero(b []byte) {
	for i := range b {
		b[i] = 0x00
	}
}
