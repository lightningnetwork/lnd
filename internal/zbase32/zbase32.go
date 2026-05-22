// Package zbase32 implements the z-base-32 encoding as specified in
// http://philzimmermann.com/docs/human-oriented-base-32-encoding.txt
//
// Note that this is NOT RFC 4648, for that see encoding/base32.
// z-base-32 is a variant that aims to be more human-friendly, and in some
// circumstances shorter.
//
// This package is an internal fork of github.com/tv42/zbase32 (originally
// authored by Tommi Virtanen, MIT-licensed) reduced to the small surface
// area that lnd uses (signed-message encoding / decoding for the
// SignMessage / VerifyMessage RPCs). It exists so that lnd does not depend
// on an external module for a small, well-defined encoding routine.
package zbase32

import "strconv"

// alphabet is the z-base-32 alphabet, which orders characters by ease of
// pronunciation rather than alphabetically (the property that differentiates
// z-base-32 from RFC 4648 base32).
const alphabet = "ybndrfg8ejkmcpqxot1uwisza345h769"

// decodeMap is the reverse lookup table: input byte -> alphabet index, or
// 0xFF for "not a valid encoded character".
var decodeMap [256]byte

func init() {
	for i := 0; i < len(decodeMap); i++ {
		decodeMap[i] = 0xFF
	}
	for i := 0; i < len(alphabet); i++ {
		decodeMap[alphabet[i]] = byte(i)
	}
}

// CorruptInputError means that the byte at this offset was not a valid
// z-base-32 encoding byte.
type CorruptInputError int64

// Error returns a human-readable description of the corrupt-input position.
func (e CorruptInputError) Error() string {
	return "illegal z-base-32 data at input byte " +
		strconv.FormatInt(int64(e), 10)
}

// EncodedLen returns the maximum length in bytes of the z-base-32 encoding
// of an input buffer of length n.
func EncodedLen(n int) int {
	return (n + 4) / 5 * 8
}

// DecodedLen returns the maximum length in bytes of the decoded data
// corresponding to n bytes of z-base-32-encoded data.
func DecodedLen(n int) int {
	return (n + 7) / 8 * 5
}

// encode emits z-base-32 characters by sliding a 5-bit window across the
// input. When bits < 0 the routine encodes len(src)*8 bits; otherwise it
// emits exactly `bits` bits' worth of output and masks any trailing bits
// past `bits` to zero.
func encode(dst, src []byte, bits int) int {
	off := 0
	for i := 0; i < bits || (bits < 0 && len(src) > 0); i += 5 {
		b0 := src[0]
		b1 := byte(0)

		if len(src) > 1 {
			b1 = src[1]
		}

		char := byte(0)
		offset := uint(i % 8)

		if offset < 4 {
			char = b0 & (31 << (3 - offset)) >> (3 - offset)
		} else {
			char = b0 & (31 >> (offset - 3)) << (offset - 3)
			char |= b1 & (255 << (11 - offset)) >> (11 - offset)
		}

		// If src is longer than necessary, mask trailing bits to zero.
		if bits >= 0 && i+5 > bits {
			char &= 255 << uint((i+5)-bits)
		}

		dst[off] = alphabet[char]
		off++

		if offset > 2 {
			src = src[1:]
		}
	}
	return off
}

// Encode encodes src. It writes at most EncodedLen(len(src)) bytes to dst
// and returns the number of bytes written.
func Encode(dst, src []byte) int {
	return encode(dst, src, -1)
}

// EncodeToString returns the z-base-32 encoding of src.
func EncodeToString(src []byte) string {
	dst := make([]byte, EncodedLen(len(src)))
	n := Encode(dst, src)
	return string(dst[:n])
}

// decode is the inverse of encode. When bits < 0 the routine consumes the
// full src buffer; otherwise it decodes exactly `bits` bits.
func decode(dst, src []byte, bits int) (int, error) {
	olen := len(src)
	off := 0
	for len(src) > 0 {
		// Decode quantum using the z-base-32 alphabet.
		var dbuf [8]byte

		j := 0
		for ; j < 8; j++ {
			if len(src) == 0 {
				break
			}
			in := src[0]
			src = src[1:]
			dbuf[j] = decodeMap[in]
			if dbuf[j] == 0xFF {
				return off, CorruptInputError(
					olen - len(src) - 1,
				)
			}
		}

		// 8x 5-bit source blocks unpack into a 5-byte destination
		// quantum.
		dst[off+0] = dbuf[0]<<3 | dbuf[1]>>2
		dst[off+1] = dbuf[1]<<6 | dbuf[2]<<1 | dbuf[3]>>4
		dst[off+2] = dbuf[3]<<4 | dbuf[4]>>1
		dst[off+3] = dbuf[4]<<7 | dbuf[5]<<2 | dbuf[6]>>3
		dst[off+4] = dbuf[6]<<5 | dbuf[7]

		// bits < 0 means "as many bits as there are in src". The
		// lookup table maps the number of input characters in the
		// current quantum to the number of output bytes produced.
		if bits < 0 {
			lookup := []int{0, 1, 1, 2, 2, 3, 4, 4, 5}
			off += lookup[j]
			continue
		}
		bitsInBlock := bits
		if bitsInBlock > 40 {
			bitsInBlock = 40
		}
		off += (bitsInBlock + 7) / 8
		bits -= 40
	}
	return off, nil
}

// Decode decodes z-base-32 encoded data from src. It writes at most
// DecodedLen(len(src)) bytes to dst and returns the number of bytes
// written. If src contains invalid data the returned error is a
// CorruptInputError.
func Decode(dst, src []byte) (int, error) {
	return decode(dst, src, -1)
}

// DecodeString returns the bytes represented by the z-base-32 string s.
func DecodeString(s string) ([]byte, error) {
	dst := make([]byte, DecodedLen(len(s)))
	n, err := decode(dst, []byte(s), -1)
	if err != nil {
		return nil, err
	}
	return dst[:n], nil
}

