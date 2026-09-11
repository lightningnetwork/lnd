package commands

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"rsc.io/qr"
)

// TestRenderTerminalQR verifies the half-block mapping and the four-module
// quiet zone around the QR code.
func TestRenderTerminalQR(t *testing.T) {
	t.Parallel()

	code := &qr.Code{
		Bitmap: []byte{
			0b11000000,
			0b10100000,
			0b11000000,
			0b10100000,
		},
		Size:   4,
		Stride: 1,
	}

	quietRow := terminalQRColors + strings.Repeat(" ", 12) +
		terminalQRReset + "\n"
	contentRow := terminalQRColors + strings.Repeat(" ", 4) + "█▀▄ " +
		strings.Repeat(" ", 4) + terminalQRReset + "\n"
	expected := strings.Repeat(quietRow, 2) +
		strings.Repeat(contentRow, 2) +
		strings.Repeat(quietRow, 2)

	require.Equal(t, expected, renderTerminalQR(code, true))
}

// TestRenderTerminalQRRealSize verifies the odd-sized QR geometry used in
// production, including fully quiet first and last terminal rows.
func TestRenderTerminalQRRealSize(t *testing.T) {
	t.Parallel()

	code, err := qr.Encode("TEST", qr.L)
	require.NoError(t, err)

	rendered := renderTerminalQR(code, true)
	lines := strings.Split(strings.TrimSuffix(rendered, "\n"), "\n")
	require.Len(t, lines, (code.Size+9)/2)

	quietRow := terminalQRColors +
		strings.Repeat(" ", code.Size+2*terminalQRQuietZone) +
		terminalQRReset
	require.Equal(t, quietRow, lines[0])
	require.Equal(t, quietRow, lines[len(lines)-1])
}

// TestWriteInvoiceTerminalQR verifies BOLT 11 uppercasing and that encoder and
// writer failures are returned to the caller.
func TestWriteInvoiceTerminalQR(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	require.NoError(t, writeInvoiceTerminalQR(&output, "test"))
	require.NotEmpty(t, output.String())
	require.NotContains(t, output.String(), "\x1b[")

	// Version 40-L can hold 4,296 alphanumeric characters. A valid lnd
	// payment request is capped at 4,096 characters, so uppercasing it must
	// remain encodable.
	require.NoError(t, writeInvoiceTerminalQR(
		&output, strings.Repeat("a", 4_096),
	))

	err := writeInvoiceTerminalQR(
		&output, strings.Repeat("a", 4_297),
	)
	require.ErrorContains(t, err, "unable to encode QR code")

	err = writeInvoiceTerminalQR(failingWriter{}, "test")
	require.ErrorContains(t, err, "unable to write QR code")
}

// TestPrintInvoiceQR verifies that presentation failures are reported without
// being returned to the command after the invoice has been created.
func TestPrintInvoiceQR(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	printInvoiceQR(&output, strings.Repeat("a", 4_297))

	require.Contains(t, output.String(), "warning: invoice created")
}

// TestValidateTerminalQRSize verifies that QR codes which would wrap or extend
// beyond the viewport are rejected with an actionable error.
func TestValidateTerminalQRSize(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateTerminalQRSize(80, 80, 41))
	require.NoError(t, validateTerminalQRSize(79, 80, 41))

	err := validateTerminalQRSize(81, 80, 42)
	require.ErrorContains(t, err, "QR code is 81 columns by 41 rows")
	require.ErrorContains(t, err, "terminal is 80 by 42")

	err = validateTerminalQRSize(79, 80, 40)
	require.ErrorContains(t, err, "QR code is 79 columns by 40 rows")
	require.ErrorContains(t, err, "terminal is 80 by 40")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}
