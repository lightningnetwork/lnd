package migration1

import (
	"encoding/binary"
	"fmt"
	"image/color"
	"io"
	"strconv"

	"github.com/btcsuite/btcd/wire"
)

var (
	// byteOrder defines the preferred byte order, which is Big Endian.
	byteOrder = binary.BigEndian
)

// WriteOutpoint writes an outpoint to the passed writer using the minimal
// amount of bytes possible.
func WriteOutpoint(w io.Writer, o *wire.OutPoint) error {
	if _, err := w.Write(o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Write(w, byteOrder, o.Index); err != nil {
		return err
	}

	return nil
}

// ReadOutpoint reads an outpoint from the passed reader that was previously
// written using the WriteOutpoint struct.
func ReadOutpoint(r io.Reader, o *wire.OutPoint) error {
	if _, err := io.ReadFull(r, o.Hash[:]); err != nil {
		return err
	}
	if err := binary.Read(r, byteOrder, &o.Index); err != nil {
		return err
	}

	return nil
}

// EncodeHexColor takes a color and returns it in hex code format.
func EncodeHexColor(color color.RGBA) string {
	return fmt.Sprintf("#%02x%02x%02x", color.R, color.G, color.B)
}

// DecodeHexColor takes a hex color string like "#rrggbb" and returns a
// color.RGBA.
func DecodeHexColor(hex string) (color.RGBA, error) {
	if len(hex) != 7 || hex[0] != '#' {
		return color.RGBA{}, fmt.Errorf("invalid hex color string: %s",
			hex)
	}

	r, err := strconv.ParseUint(hex[1:3], 16, 8)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("invalid red component: %w",
			err)
	}

	g, err := strconv.ParseUint(hex[3:5], 16, 8)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("invalid green component: %w",
			err)
	}

	b, err := strconv.ParseUint(hex[5:7], 16, 8)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("invalid blue component: %w",
			err)
	}

	return color.RGBA{
		R: uint8(r),
		G: uint8(g),
		B: uint8(b),
	}, nil
}
