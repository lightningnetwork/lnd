package aezeed

import "fmt"

var (
	// ErrIncorrectVersion is returned if a seed bares a mismatched
	// external version to that of the package executing the aezeed scheme.
	ErrIncorrectVersion = fmt.Errorf("wrong seed version")

	// ErrInvalidPass is returned if the user enters an invalid passphrase
	// for a particular enciphered mnemonic.
	ErrInvalidPass = fmt.Errorf("invalid passphrase")

	// ErrIncorrectMnemonic is returned if we detect that the checksum of
	// the specified mnemonic doesn't match. This indicates the user input
	// the wrong mnemonic.
	ErrIncorrectMnemonic = fmt.Errorf("mnemonic phrase checksum doesn't " +
		"match")
)

// ErrUnknownMnenomicWord is returned when attempting to decipher and
// enciphered mnemonic, but a word encountered isn't a member of our word list.
type ErrUnknownMnenomicWord struct {
	// Word is the unknown word in the mnemonic phrase.
	Word string

	// Index is the index (starting from zero) within the slice of strings
	// that makes up the mnemonic that points to the incorrect word.
	Index uint8
}

// Error returns a human readable string describing the error.
func (e ErrUnknownMnenomicWord) Error() string {
	return fmt.Sprintf("word %v isn't a part of default word list "+
		"(index=%v)", e.Word, e.Index)
}
