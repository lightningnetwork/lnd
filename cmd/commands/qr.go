package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/urfave/cli"
	"golang.org/x/term"
	"rsc.io/qr"
)

const (
	terminalQRQuietZone = 4

	// Pin the foreground and background colors so the QR code has the
	// required dark-on-light polarity regardless of the terminal theme.
	terminalQRColors = "\x1b[30;107m"
	terminalQRReset  = "\x1b[0m"
)

var invoiceQRFlag = cli.BoolFlag{
	Name: "qr",
	Usage: "display a QR code of the payment request " +
		"in the terminal",
}

// maybePrintInvoiceQR prints an invoice's payment request as a QR code if the
// user requested it. Rendering is presentation-only, so a failure is reported
// without turning a successfully created invoice into a failed command.
func maybePrintInvoiceQR(ctx *cli.Context, paymentRequest string) {
	if !ctx.Bool(invoiceQRFlag.Name) {
		return
	}

	printInvoiceQR(os.Stderr, paymentRequest)
}

// printInvoiceQR prints an invoice QR code to w, reporting any presentation
// failure to the same writer.
func printInvoiceQR(w io.Writer, paymentRequest string) {
	_, _ = fmt.Fprintln(w)

	err := writeInvoiceTerminalQR(w, paymentRequest)
	if err != nil {
		_, _ = fmt.Fprintf(
			w, "warning: invoice created, but its QR code could "+
				"not be displayed: %v\n", err,
		)
	}
}

// writeInvoiceTerminalQR encodes a BOLT 11 payment request as a
// low-error-correction QR code and writes a compact, half-block representation
// of it to w. BOLT 11 recommends upper case for QR encoding so the more compact
// QR alphanumeric mode is used.
func writeInvoiceTerminalQR(w io.Writer, paymentRequest string) error {
	code, err := qr.Encode(strings.ToUpper(paymentRequest), qr.L)
	if err != nil {
		return fmt.Errorf("unable to encode QR code: %w", err)
	}

	width := code.Size + 2*terminalQRQuietZone
	useColors, err := checkTerminalQRSize(w, width)
	if err != nil {
		return err
	}

	_, err = io.WriteString(w, renderTerminalQR(code, useColors))
	if err != nil {
		return fmt.Errorf("unable to write QR code: %w", err)
	}

	return nil
}

// checkTerminalQRSize reports whether terminal colors should be used and makes
// sure a QR code written directly to a terminal fits in its viewport.
// Non-terminal writers have no intrinsic display size, so they are left
// untouched and rendered without terminal escape sequences.
func checkTerminalQRSize(w io.Writer, qrWidth int) (bool, error) {
	fdWriter, ok := w.(interface {
		Fd() uintptr
	})
	if !ok {
		return false, nil
	}

	fd := int(fdWriter.Fd())
	if !term.IsTerminal(fd) {
		return false, nil
	}

	terminalWidth, terminalHeight, err := term.GetSize(fd)
	if err != nil {
		return true, nil
	}

	return true, validateTerminalQRSize(
		qrWidth, terminalWidth, terminalHeight,
	)
}

// validateTerminalQRSize returns an actionable error when a QR code would wrap
// or extend beyond the terminal viewport.
func validateTerminalQRSize(qrWidth, terminalWidth, terminalHeight int) error {
	qrHeight := (qrWidth + 1) / 2

	// Leave one terminal row for the shell prompt printed after the
	// command.
	if qrWidth <= terminalWidth && qrHeight < terminalHeight {
		return nil
	}

	return fmt.Errorf("QR code is %d columns by %d rows, but the terminal "+
		"is %d by %d; resize the terminal or reduce the font size",
		qrWidth, qrHeight, terminalWidth, terminalHeight)
}

// renderTerminalQR renders two QR module rows per terminal row. White modules
// use a fixed white background and black modules use a fixed black foreground
// when withColors is true, making the QR code's polarity independent of the
// terminal theme.
func renderTerminalQR(code *qr.Code, withColors bool) string {
	const (
		bothBlack   = '█'
		topBlack    = '▀'
		bottomBlack = '▄'
		bothWhite   = ' '
	)

	size := code.Size + 2*terminalQRQuietZone
	height := (size + 1) / 2

	var result strings.Builder
	result.Grow(height * (len(terminalQRColors) + size*3 +
		len(terminalQRReset) + 1))

	lowerBound := -terminalQRQuietZone
	upperBound := code.Size + terminalQRQuietZone
	for y := lowerBound; y < upperBound; y += 2 {
		if withColors {
			result.WriteString(terminalQRColors)
		}

		for x := lowerBound; x < upperBound; x++ {
			top := code.Black(x, y)
			bottom := code.Black(x, y+1)

			switch {
			case top && bottom:
				result.WriteRune(bothBlack)

			case top:
				result.WriteRune(topBlack)

			case bottom:
				result.WriteRune(bottomBlack)

			default:
				result.WriteRune(bothWhite)
			}
		}

		if withColors {
			result.WriteString(terminalQRReset)
		}

		result.WriteByte('\n')
	}

	return result.String()
}
