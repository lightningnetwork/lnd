// Package bstream provides a minimal bit-level stream reader/writer used by
// the aezeed cipher seed encoding. It is an internal fork of
// github.com/kkdai/bstream reduced to the call surface aezeed actually uses
// (NewBStreamReader, NewBStreamWriter, ReadBits, WriteBits, Bytes).
//
// IMPORTANT: this package implements the exact bit-packing scheme used to
// serialize aezeed cipher seeds. The wire format is consensus-frozen — any
// change to the bit ordering or padding semantics here will silently
// invalidate every existing 24-word seed mnemonic in the wild. Do not
// modify the algorithm; only refactor.
package bstream

import "io"

// bit is a typed boolean so the API matches the upstream package and the
// WriteBit / ReadBit semantics stay self-documenting.
type bit bool

// BStream is a bit-addressable buffer used to pack and unpack
// non-byte-aligned values (aezeed uses 11-bit indices into the mnemonic
// word list).
type BStream struct {
	stream []byte

	// rCount is the number of bits still unread from the first byte of
	// the stream.
	rCount uint8

	// wCount is the number of bits still empty in the last byte of the
	// stream.
	wCount uint8
}

// NewBStreamReader wraps an existing buffer for reading.
func NewBStreamReader(data []byte) *BStream {
	return &BStream{stream: data, rCount: 8}
}

// NewBStreamWriter pre-allocates a buffer with the given byte capacity for
// writing.
func NewBStreamWriter(nByte uint8) *BStream {
	return &BStream{stream: make([]byte, 0, nByte), rCount: 8}
}

// writeBit appends a single bit to the stream, allocating a new byte when
// the trailing byte is full.
func (b *BStream) writeBit(input bit) {
	if b.wCount == 0 {
		b.stream = append(b.stream, 0)
		b.wCount = 8
	}

	latestIndex := len(b.stream) - 1
	if input {
		b.stream[latestIndex] |= 1 << (b.wCount - 1)
	}
	b.wCount--
}

// writeOneByte appends an 8-bit value, handling the unaligned case where
// the trailing byte still has wCount free bits.
func (b *BStream) writeOneByte(data byte) {
	if b.wCount == 0 {
		b.stream = append(b.stream, data)
		return
	}

	latestIndex := len(b.stream) - 1

	b.stream[latestIndex] |= data >> (8 - b.wCount)
	b.stream = append(b.stream, 0)
	latestIndex++
	b.stream[latestIndex] = data << b.wCount
}

// WriteBits appends the low `count` bits of `data` to the stream, MSB
// first.
func (b *BStream) WriteBits(data uint64, count int) {
	data <<= uint(64 - count)

	// Handle full bytes first so the per-bit loop only runs for the
	// trailing remainder.
	for count >= 8 {
		byt := byte(data >> (64 - 8))
		b.writeOneByte(byt)

		data <<= 8
		count -= 8
	}

	for count > 0 {
		bi := data >> (64 - 1)
		b.writeBit(bi == 1)

		data <<= 1
		count--
	}
}

// readBit consumes a single bit from the front of the stream.
func (b *BStream) readBit() (bit, error) {
	if len(b.stream) == 0 {
		return false, io.EOF
	}

	// If the first byte is exhausted, advance to the next byte.
	if b.rCount == 0 {
		b.stream = b.stream[1:]

		if len(b.stream) == 0 {
			return false, io.EOF
		}

		b.rCount = 8
	}

	retBit := b.stream[0] & (1 << (b.rCount - 1))
	b.rCount--

	return retBit != 0, nil
}

// readByte consumes a full 8-bit value from the stream, handling the
// unaligned case where the byte straddles two stream bytes.
func (b *BStream) readByte() (byte, error) {
	if len(b.stream) == 0 {
		return 0, io.EOF
	}

	if b.rCount == 0 {
		b.stream = b.stream[1:]

		if len(b.stream) == 0 {
			return 0, io.EOF
		}

		b.rCount = 8
	}

	// Aligned case: the next 8 bits coincide with a byte boundary.
	if b.rCount == 8 {
		byt := b.stream[0]
		b.stream = b.stream[1:]
		return byt, nil
	}

	retByte := b.stream[0] << (8 - b.rCount)
	b.stream = b.stream[1:]

	if len(b.stream) == 0 {
		return 0, io.EOF
	}

	retByte |= b.stream[0] >> b.rCount
	return retByte, nil
}

// ReadBits consumes the next `count` bits from the stream into the low
// bits of the returned uint64, MSB first.
func (b *BStream) ReadBits(count int) (uint64, error) {
	var retValue uint64

	for count >= 8 {
		retValue <<= 8
		byt, err := b.readByte()
		if err != nil {
			return 0, err
		}
		retValue |= uint64(byt)
		count -= 8
	}

	for count > 0 {
		retValue <<= 1
		bi, err := b.readBit()
		if err != nil {
			return 0, err
		}
		if bi {
			retValue |= 1
		}

		count--
	}

	return retValue, nil
}

// Bytes returns the backing buffer. For writers this is the packed
// output; for readers it is whatever input remains unread.
func (b *BStream) Bytes() []byte {
	return b.stream
}
