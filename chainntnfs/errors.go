package chainntnfs

import "errors"

var (
	// ErrCorruptedHeightHintCache indicates that the on-disk representation
	// has altered since the height hint cache instance was initialized.
	ErrCorruptedHeightHintCache = errors.New("height hint cache has been " +
		"corrupted")

	// ErrSpendHintNotFound is an error returned when a spend hint for an
	// outpoint was not found.
	ErrSpendHintNotFound = errors.New("spend hint not found")

	// ErrConfirmHintNotFound is an error returned when a confirm hint for a
	// transaction was not found.
	ErrConfirmHintNotFound = errors.New("confirm hint not found")
)
