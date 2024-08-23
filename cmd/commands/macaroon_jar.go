package commands

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/btcsuite/btcwallet/snacl"
	"gopkg.in/macaroon.v2"
)

const (
	encryptionPrefix = "snacl:"
)

// getPasswordFn is a function that asks the user to type a password after
// presenting it the given prompt.
type getPasswordFn func(prompt string) ([]byte, error)

// macaroonJar is a struct that represents all macaroons of a profile.
type macaroonJar struct {
	Default string           `json:"default,omitempty"`
	Timeout int64            `json:"timeout,omitempty"`
	IP      string           `json:"ip,omitempty"`
	Jar     []*macaroonEntry `json:"jar"`
}

// macaroonEntry is a struct that represents a single macaroon. Its content can
// either be cleartext (hex encoded) or encrypted (snacl secretbox).
type macaroonEntry struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

// loadMacaroon returns the fully usable macaroon instance from the entry. This
// detects whether the macaroon needs to be decrypted and does so if necessary.
// An encrypted macaroon that needs to be decrypted will prompt for the user's
// password by calling the provided password callback. Normally that should
// result in the user being prompted for the password in the terminal.
func (e *macaroonEntry) loadMacaroon(
	pwCallback getPasswordFn) (*macaroon.Macaroon, error) {

	if len(strings.TrimSpace(e.Data)) == 0 {
		return nil, fmt.Errorf("macaroon data is empty")
	}

	var (
		macBytes []byte
		err      error
	)

	// Either decrypt or simply decode the macaroon data.
	if strings.HasPrefix(e.Data, encryptionPrefix) {
		parts := strings.Split(e.Data, ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid encrypted macaroon " +
				"format, expected 'snacl:<key_base64>:" +
				"<encrypted_macaroon_base64>'")
		}

		pw, err := pwCallback("Enter macaroon encryption password: ")
		if err != nil {
			return nil, fmt.Errorf("could not read password from "+
				"terminal: %v", err)
		}

		macBytes, err = decryptMacaroon(parts[1], parts[2], pw)
		if err != nil {
			return nil, fmt.Errorf("unable to decrypt macaroon: %w",
				err)
		}
	} else {
		macBytes, err = hex.DecodeString(e.Data)
		if err != nil {
			return nil, fmt.Errorf("unable to hex decode "+
				"macaroon: %v", err)
		}
	}

	// Parse the macaroon data into its native struct.
	mac := &macaroon.Macaroon{}
	if err := mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %w", err)
	}
	return mac, nil
}

// storeMacaroon stores a native macaroon instance to the entry. If a non-nil
// password is provided, then the macaroon is encrypted with that password. If
// not, the macaroon is stored as plain text.
func (e *macaroonEntry) storeMacaroon(mac *macaroon.Macaroon, pw []byte) error {
	// First of all, make sure we can serialize the macaroon.
	macBytes, err := mac.MarshalBinary()
	if err != nil {
		return fmt.Errorf("unable to marshal macaroon: %w", err)
	}

	if len(pw) == 0 {
		e.Data = hex.EncodeToString(macBytes)
		return nil
	}

	// The user did set a password. Let's derive an encryption key from it.
	key, err := snacl.NewSecretKey(
		&pw, snacl.DefaultN, snacl.DefaultR, snacl.DefaultP,
	)
	if err != nil {
		return fmt.Errorf("unable to create encryption key: %w", err)
	}

	// Encrypt the macaroon data with the derived key and store it in the
	// human readable format snacl:<key_base64>:<encrypted_macaroon_base64>.
	encryptedMac, err := key.Encrypt(macBytes)
	if err != nil {
		return fmt.Errorf("unable to encrypt macaroon: %w", err)
	}

	keyB64 := base64.StdEncoding.EncodeToString(key.Marshal())
	dataB64 := base64.StdEncoding.EncodeToString(encryptedMac)
	e.Data = fmt.Sprintf("%s%s:%s", encryptionPrefix, keyB64, dataB64)

	return nil
}

// decryptMacaroon decrypts the cipher text macaroon by using the serialized
// encryption key and the password.
func decryptMacaroon(keyB64, dataB64 string, pw []byte) ([]byte, error) {
	// Base64 decode both the marshalled encryption key and macaroon data.
	keyData, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("could not base64 decode encryption "+
			"key: %v", err)
	}
	encryptedMac, err := base64.StdEncoding.DecodeString(dataB64)
	if err != nil {
		return nil, fmt.Errorf("could not base64 decode macaroon "+
			"data: %v", err)
	}

	// Unmarshal the encryption key and ask the user for the password.
	key := &snacl.SecretKey{}
	err = key.Unmarshal(keyData)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal encryption key: %w",
			err)
	}

	// Derive the final encryption key and then decrypt the macaroon with
	// it.
	err = key.DeriveKey(&pw)
	if err != nil {
		return nil, fmt.Errorf("could not derive encryption key, "+
			"possibly due to incorrect password: %v", err)
	}
	macBytes, err := key.Decrypt(encryptedMac)
	if err != nil {
		return nil, fmt.Errorf("could not decrypt macaroon data: %w",
			err)
	}
	return macBytes, nil
}
