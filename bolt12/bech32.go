package bolt12

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/btcsuite/btcd/btcutil/bech32"
)

var (
	// ErrStringTooLong is returned when a string is longer than
	// maxBolt12StringLen. It is also returned when a payload is larger than
	// maxBolt12DataLen.
	ErrStringTooLong = errors.New("input length exceeds limit")

	// ErrEmptyString is returned when a string has no characters. It is
	// also returned when a payload has no bytes.
	ErrEmptyString = errors.New("empty string")

	// ErrMixedCase is returned when a bech32 string contains both
	// uppercase and lowercase characters.
	ErrMixedCase = errors.New("string not all lowercase or all uppercase")

	// ErrInvalidSeparator is returned when the '1' separator is missing
	// or misplaced.
	ErrInvalidSeparator = errors.New("missing or invalid separator")

	// ErrUnsupportedHRP is returned when the human-readable prefix is not
	// in validHRPs (lno/lnr/lni).
	ErrUnsupportedHRP = errors.New("unsupported HRP")

	// ErrInvalidCharacter is returned when a character outside printable
	// ASCII or outside the bech32 charset is encountered.
	ErrInvalidCharacter = errors.New("invalid character")

	// ErrInvalidContinuation is returned when '+' placement violates BOLT
	// 12 rules.
	ErrInvalidContinuation = errors.New("invalid continuation")

	// ErrBaseConversion is returned when base 32 / base 256 conversion
	// fails.
	ErrBaseConversion = errors.New("base conversion failed")

	// ErrCharConversion is returned when a 5-bit value exceeds the bech32
	// alphabet bounds.
	ErrCharConversion = errors.New("char conversion failed")
)

const (
	// HRPOffer is the human-readable prefix for BOLT 12 offers.
	HRPOffer = "lno"

	// HRPInvoiceRequest is the human-readable prefix for BOLT 12 invoice
	// requests.
	HRPInvoiceRequest = "lnr"

	// HRPInvoice is the human-readable prefix for BOLT 12 invoices.
	HRPInvoice = "lni"

	// charset is the set of valid bech32 characters.
	charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

	// minPrintableASCII is the lower bound for printable ASCII characters
	// ('!').
	minPrintableASCII = 33

	// maxPrintableASCII is the upper bound for printable ASCII characters
	// ('~').
	maxPrintableASCII = 126

	// bolt12HRPLen is the length of a BOLT 12 human-readable prefix. All
	// three prefixes have it, so the limit below counts it as a fixed cost.
	bolt12HRPLen = 3

	// maxBolt12DataLen is the largest TLV stream that one BOLT 12 string
	// can hold. The spec limits neither a field nor the stream, so the
	// limit comes from this package: the P2P decoder rejects a record above
	// tlv.MaxRecordSize. Eleven offer fields at that size give 704
	// kibibytes, and one mebibyte leaves room for unknown odd fields. Only
	// an offer needs the room, because an invoice travels in a smaller
	// onion message.
	maxBolt12DataLen = 1 << 20

	// maxBolt12StringLen is the largest BOLT 12 bech32 string the codec
	// accepts. Each character of the data part holds 5 of the 8 bits of a
	// payload byte. The limit therefore comes from maxBolt12DataLen. It
	// counts the prefix, the separator, and one character for each group of
	// 5 bits. Encode and Decode use the same limit, so every string that
	// Encode makes is a string that Decode accepts.
	maxBolt12StringLen = bolt12HRPLen + 1 + (maxBolt12DataLen*8+4)/5
)

// validHRPs holds the prefixes the BOLT 12 codec accepts, in the order the
// error messages name them.
var validHRPs = []string{HRPOffer, HRPInvoiceRequest, HRPInvoice}

// isValidHRP tells the caller if hrp is a BOLT 12 prefix.
func isValidHRP(hrp string) bool {
	return slices.Contains(validHRPs, hrp)
}

// unsupportedHRPError reports that hrp is not a BOLT 12 prefix. The message
// names the permitted prefixes from the one list that holds them.
func unsupportedHRPError(hrp string) error {
	return fmt.Errorf(
		"bolt12: %w %q (want %s)", ErrUnsupportedHRP, hrp,
		strings.Join(validHRPs, "/"),
	)
}

// Decode reads a BOLT 12 bech32 string. It returns the human-readable prefix
// and the data bytes. A BOLT 12 string has no checksum. A '+' character can
// join two parts of the string, and whitespace can follow it. Decode rejects a
// string above maxBolt12StringLen, but the caller must set a smaller limit for
// its own medium. See the caller obligations in the package documentation.
func Decode(s string) (string, []byte, error) {
	if len(s) > maxBolt12StringLen {
		return "", nil, fmt.Errorf(
			"bolt12: %w: input length %d exceeds limit %d",
			ErrStringTooLong, len(s), maxBolt12StringLen,
		)
	}

	cleaned, err := stripContinuation(s)
	if err != nil {
		return "", nil, err
	}

	if len(cleaned) == 0 {
		return "", nil, fmt.Errorf("bolt12: %w", ErrEmptyString)
	}

	// The characters must be either all lowercase or all uppercase.
	lower := strings.ToLower(cleaned)
	if cleaned != lower && cleaned != strings.ToUpper(cleaned) {
		return "", nil, fmt.Errorf("bolt12: %w", ErrMixedCase)
	}

	cleaned = lower

	// Find the separator. The last '1' separates the HRP from data.
	one := strings.LastIndexByte(cleaned, '1')
	if one < 1 || one+1 >= len(cleaned) {
		return "", nil, fmt.Errorf("bolt12: %w", ErrInvalidSeparator)
	}

	hrp := cleaned[:one]
	if !isValidHRP(hrp) {
		return "", nil, unsupportedHRPError(hrp)
	}
	dataStr := cleaned[one+1:]

	// Validate and convert each character to its bech32 value.
	data5bit, err := toBech32Bytes(dataStr)
	if err != nil {
		return "", nil, err
	}

	// Convert from base32 (5-bit groups) to base256 (8-bit bytes).
	data8bit, err := bech32.ConvertBits(data5bit, 5, 8, false)
	if err != nil {
		return "", nil, fmt.Errorf(
			"bolt12: %w: %w", ErrBaseConversion, err,
		)
	}

	return hrp, data8bit, nil
}

// Encode makes a BOLT 12 bech32 string from the data bytes and the given
// human-readable prefix. It adds no checksum. It changes the prefix to
// lowercase and takes only lno, lnr, and lni. The payload size must be a size
// that Decode also takes, so a caller can make only strings that Decode reads.
func Encode(hrp string, data []byte) (string, error) {
	hrp = strings.ToLower(hrp)
	if !isValidHRP(hrp) {
		return "", unsupportedHRPError(hrp)
	}

	// A BOLT 12 string holds a TLV stream, and the stream must hold at
	// least one record. An empty payload gives a string with only the
	// prefix and the separator, which Decode rejects.
	if len(data) == 0 {
		return "", fmt.Errorf(
			"bolt12: %w: nothing to encode", ErrEmptyString,
		)
	}

	if len(data) > maxBolt12DataLen {
		return "", fmt.Errorf(
			"bolt12: %w: payload length %d exceeds limit %d",
			ErrStringTooLong, len(data), maxBolt12DataLen,
		)
	}

	// Convert from base256 to base32.
	data5bit, err := bech32.ConvertBits(data, 8, 5, true)
	if err != nil {
		return "", fmt.Errorf("bolt12: %w: %w", ErrBaseConversion, err)
	}

	chars, err := toBech32Chars(data5bit)
	if err != nil {
		return "", fmt.Errorf("bolt12: %w: %w", ErrCharConversion, err)
	}

	return hrp + "1" + chars, nil
}

// stripContinuation removes each '+' marker and the whitespace after it, and
// rejects each byte outside the printable ASCII range. A marker joins two parts
// of one string, so a character that is neither whitespace nor a second marker
// must stand on each side. This rejects a marker at the start or the end, and
// two markers together.
//
// The two characters need not be bech32 characters. The spec does not say what
// to do inside the prefix, and the prefix check and the alphabet scan run after
// this step, so a marker there cannot make an invalid string valid.
func stripContinuation(s string) (string, error) {
	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '+' {
			if c < minPrintableASCII || c > maxPrintableASCII {
				return "", fmt.Errorf(
					"bolt12: %w: invalid byte 0x%02x at "+
						"position %d",
					ErrInvalidCharacter, c, i,
				)
			}
			b.WriteByte(c)

			continue
		}

		if i == 0 || !isContinuationNeighbour(s[i-1]) {
			return "", fmt.Errorf(
				"bolt12: %w: '+' must follow a "+
					"non-whitespace character",
				ErrInvalidContinuation,
			)
		}

		// Skip '+' and any following whitespace.
		j := i + 1
		for j < len(s) && isWhitespace(s[j]) {
			j++
		}
		if j >= len(s) || !isContinuationNeighbour(s[j]) {
			return "", fmt.Errorf(
				"bolt12: %w: '+' must precede a "+
					"non-whitespace character",
				ErrInvalidContinuation,
			)
		}

		// Resume at the character the '+' joined to.
		i = j - 1
	}

	return b.String(), nil
}

// isContinuationNeighbour tells the caller if c can stand next to a '+' marker.
// A marker joins string content, so whitespace and a second marker cannot.
func isContinuationNeighbour(c byte) bool {
	return c != '+' && !isWhitespace(c)
}

// isWhitespace tells the caller if c is one of the six ASCII whitespace
// characters: space, tab, line feed, vertical tab, form feed, and carriage
// return. The spec narrows the class nowhere, and this is the set in
// strings.asciiSpace. unicode.IsSpace is the wrong test here, because it also
// accepts the byte 0x85 and the byte 0xA0, which a BOLT 12 string cannot
// hold.
func isWhitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\v' ||
		c == '\f' || c == '\r'
}

// toBech32Bytes converts a string of bech32 characters to their 5-bit integer
// values. Reported position offsets are relative to the normalized string after
// continuation stripping.
func toBech32Bytes(s string) ([]byte, error) {
	result := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		idx := strings.IndexByte(charset, s[i])
		if idx < 0 {
			return nil, fmt.Errorf(
				"bolt12: %w: invalid character 0x%02x at "+
					"position %d of the cleaned data "+
					"string %s",
				ErrInvalidCharacter, s[i], i, s,
			)
		}
		result[i] = byte(idx)
	}

	return result, nil
}

// toBech32Chars converts 5-bit values to their bech32 character representation.
func toBech32Chars(data []byte) (string, error) {
	result := make([]byte, len(data))
	for i, b := range data {
		if int(b) >= len(charset) {
			return "", fmt.Errorf(
				"bolt12: %w: invalid data byte: %d",
				ErrCharConversion, b,
			)
		}
		result[i] = charset[b]
	}

	return string(result), nil
}
