// Package rpcclientallowlist parses RPC client certificate fingerprints.
package rpcclientallowlist

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Parse returns the fingerprints in a list. Blank lines and comments are
// ignored.
func Parse(r io.Reader) (map[[sha256.Size]byte]struct{}, error) {
	result := make(map[[sha256.Size]byte]struct{})
	scanner := bufio.NewScanner(r)
	for line := 1; scanner.Scan(); line++ {
		entry, _, _ := strings.Cut(scanner.Text(), "#")
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		var fingerprint [sha256.Size]byte
		if len(entry) != hex.EncodedLen(len(fingerprint)) {
			return nil, fmt.Errorf(
				"line %d: %w", line, errors.New(
					"expected a 64-digit hex "+
						"SHA-256 fingerprint",
				),
			)
		}
		_, err := hex.Decode(fingerprint[:], []byte(entry))
		if err != nil {
			return nil, fmt.Errorf(
				"line %d: expected a 64-digit hex "+
					"SHA-256 fingerprint",
				line,
			)
		}
		result[fingerprint] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}

	return result, nil
}
