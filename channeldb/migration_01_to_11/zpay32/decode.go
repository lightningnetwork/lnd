package zpay32

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	lnwire "github.com/lightningnetwork/lnd/channeldb/migration/lnwire21"
)

// Decode parses the provided encoded invoice and returns a decoded Invoice if
// it is valid by BOLT-0011 and matches the provided active network.
func Decode(invoice string, net *chaincfg.Params) (*Invoice, error) {
	decodedInvoice := Invoice{}

	// Before bech32 decoding the invoice, make sure that it is not too large.
	// This is done as an anti-DoS measure since bech32 decoding is expensive.
	if len(invoice) > maxInvoiceLength {
		return nil, ErrInvoiceTooLarge
	}

	// Decode the invoice using the modified bech32 decoder.
	hrp, data, err := decodeBech32(invoice)
	if err != nil {
		return nil, err
	}

	// We expect the human-readable part to at least have ln + one char
	// encoding the network.
	if len(hrp) < 3 {
		return nil, fmt.Errorf("hrp too short")
	}

	// First two characters of HRP should be "ln".
	if hrp[:2] != "ln" {
		return nil, fmt.Errorf("prefix should be \"ln\"")
	}

	// The next characters should be a valid prefix for a segwit BIP173
	// address that match the active network.
	if !strings.HasPrefix(hrp[2:], net.Bech32HRPSegwit) {
		return nil, fmt.Errorf(
			"invoice not for current active network '%s'", net.Name)
	}
	decodedInvoice.Net = net

	// Optionally, if there's anything left of the HRP after ln + the segwit
	// prefix, we try to decode this as the payment amount.
	var netPrefixLength = len(net.Bech32HRPSegwit) + 2
	if len(hrp) > netPrefixLength {
		amount, err := decodeAmount(hrp[netPrefixLength:])
		if err != nil {
			return nil, err
		}
		decodedInvoice.MilliSat = &amount
	}

	// Everything except the last 520 bits of the data encodes the invoice's
	// timestamp and tagged fields.
	if len(data) < signatureBase32Len {
		return nil, errors.New("short invoice")
	}
	invoiceData := data[:len(data)-signatureBase32Len]

	// Parse the timestamp and tagged fields, and fill the Invoice struct.
	if err := parseData(&decodedInvoice, invoiceData, net); err != nil {
		return nil, err
	}

	// The last 520 bits (104 groups) make up the signature.
	sigBase32 := data[len(data)-signatureBase32Len:]
	sigBase256, err := bech32.ConvertBits(sigBase32, 5, 8, true)
	if err != nil {
		return nil, err
	}
	var sig lnwire.Sig
	copy(sig[:], sigBase256[:64])
	recoveryID := sigBase256[64]

	// The signature is over the hrp + the data the invoice, encoded in
	// base 256.
	taggedDataBytes, err := bech32.ConvertBits(invoiceData, 5, 8, true)
	if err != nil {
		return nil, err
	}

	toSign := append([]byte(hrp), taggedDataBytes...)

	// We expect the signature to be over the single SHA-256 hash of that
	// data.
	hash := chainhash.HashB(toSign)

	// If the destination pubkey was provided as a tagged field, use that
	// to verify the signature, if not do public key recovery.
	if decodedInvoice.Destination != nil {
		signature, err := sig.ToSignature()
		if err != nil {
			return nil, fmt.Errorf("unable to deserialize "+
				"signature: %v", err)
		}
		if !signature.Verify(hash, decodedInvoice.Destination) {
			return nil, fmt.Errorf("invalid invoice signature")
		}
	} else {
		headerByte := recoveryID + 27 + 4
		compactSign := append([]byte{headerByte}, sig[:]...)
		pubkey, _, err := ecdsa.RecoverCompact(compactSign, hash)
		if err != nil {
			return nil, err
		}
		decodedInvoice.Destination = pubkey
	}

	// If no feature vector was decoded, populate an empty one.
	if decodedInvoice.Features == nil {
		decodedInvoice.Features = lnwire.NewFeatureVector(
			nil, lnwire.Features,
		)
	}

	// Now that we have created the invoice, make sure it has the required
	// fields set.
	if err := validateInvoice(&decodedInvoice); err != nil {
		return nil, err
	}

	return &decodedInvoice, nil
}

// parseData parses the data part of the invoice. It expects base32 data
// returned from the bech32.Decode method, except signature.
func parseData(invoice *Invoice, data []byte, net *chaincfg.Params) error {
	// It must contain the timestamp, encoded using 35 bits (7 groups).
	if len(data) < timestampBase32Len {
		return fmt.Errorf("data too short: %d", len(data))
	}

	t, err := parseTimestamp(data[:timestampBase32Len])
	if err != nil {
		return err
	}
	invoice.Timestamp = time.Unix(int64(t), 0)

	// The rest are tagged parts.
	tagData := data[7:]
	return parseTaggedFields(invoice, tagData, net)
}

// parseTimestamp converts a 35-bit timestamp (encoded in base32) to uint64.
func parseTimestamp(data []byte) (uint64, error) {
	if len(data) != timestampBase32Len {
		return 0, fmt.Errorf("timestamp must be 35 bits, was %d",
			len(data)*5)
	}

	return base32ToUint64(data)
}

// parseTaggedFields takes the base32 encoded tagged fields of the invoice, and
// fills the Invoice struct accordingly.
func parseTaggedFields(invoice *Invoice, fields []byte, net *chaincfg.Params) error {
	index := 0
	for len(fields)-index > 0 {
		// If there are less than 3 groups to read, there cannot be more
		// interesting information, as we need the type (1 group) and
		// length (2 groups).
		//
		// This means the last tagged field is broken.
		if len(fields)-index < 3 {
			return ErrBrokenTaggedField
		}

		typ := fields[index]
		dataLength, err := parseFieldDataLength(fields[index+1 : index+3])
		if err != nil {
			return err
		}

		// If we don't have enough field data left to read this length,
		// return error.
		if len(fields) < index+3+int(dataLength) {
			return ErrInvalidFieldLength
		}
		base32Data := fields[index+3 : index+3+int(dataLength)]

		// Advance the index in preparation for the next iteration.
		index += 3 + int(dataLength)

		switch typ {
		case fieldTypeP:
			if invoice.PaymentHash != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.PaymentHash, err = parse32Bytes(base32Data)
		case fieldTypeS:
			if invoice.PaymentAddr != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.PaymentAddr, err = parse32Bytes(base32Data)
		case fieldTypeD:
			if invoice.Description != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.Description, err = parseDescription(base32Data)
		case fieldTypeN:
			if invoice.Destination != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.Destination, err = parseDestination(base32Data)
		case fieldTypeH:
			if invoice.DescriptionHash != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.DescriptionHash, err = parse32Bytes(base32Data)
		case fieldTypeX:
			if invoice.expiry != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.expiry, err = parseExpiry(base32Data)
		case fieldTypeC:
			if invoice.minFinalCLTVExpiry != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.minFinalCLTVExpiry, err = parseMinFinalCLTVExpiry(base32Data)
		case fieldTypeF:
			if invoice.FallbackAddr != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.FallbackAddr, err = parseFallbackAddr(base32Data, net)
		case fieldTypeR:
			// An `r` field can be included in an invoice multiple
			// times, so we won't skip it if we have already seen
			// one.
			routeHint, err := parseRouteHint(base32Data)
			if err != nil {
				return err
			}

			invoice.RouteHints = append(invoice.RouteHints, routeHint)
		case fieldType9:
			if invoice.Features != nil {
				// We skip the field if we have already seen a
				// supported one.
				continue
			}

			invoice.Features, err = parseFeatures(base32Data)
		default:
			// Ignore unknown type.
		}

		// Check if there was an error from parsing any of the tagged
		// fields and return it.
		if err != nil {
			return err
		}
	}

	return nil
}

// parseFieldDataLength converts the two byte slice into a uint16.
func parseFieldDataLength(data []byte) (uint16, error) {
	if len(data) != 2 {
		return 0, fmt.Errorf("data length must be 2 bytes, was %d",
			len(data))
	}

	return uint16(data[0])<<5 | uint16(data[1]), nil
}

// parse32Bytes converts a 256-bit value (encoded in base32) to *[32]byte. This
// can be used for payment hashes, description hashes, payment addresses, etc.
func parse32Bytes(data []byte) (*[32]byte, error) {
	var paymentHash [32]byte

	// As BOLT-11 states, a reader must skip over the 32-byte fields if
	// it does not have a length of 52, so avoid returning an error.
	if len(data) != hashBase32Len {
		return nil, nil
	}

	hash, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}

	copy(paymentHash[:], hash)

	return &paymentHash, nil
}

// parseDescription converts the data (encoded in base32) into a string to use
// as the description.
func parseDescription(data []byte) (*string, error) {
	base256Data, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}

	description := string(base256Data)

	return &description, nil
}

// parseDestination converts the data (encoded in base32) into a 33-byte public
// key of the payee node.
func parseDestination(data []byte) (*btcec.PublicKey, error) {
	// As BOLT-11 states, a reader must skip over the destination field
	// if it does not have a length of 53, so avoid returning an error.
	if len(data) != pubKeyBase32Len {
		return nil, nil
	}

	base256Data, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}

	return btcec.ParsePubKey(base256Data)
}

// parseExpiry converts the data (encoded in base32) into the expiry time.
func parseExpiry(data []byte) (*time.Duration, error) {
	expiry, err := base32ToUint64(data)
	if err != nil {
		return nil, err
	}

	duration := time.Duration(expiry) * time.Second

	return &duration, nil
}

// parseMinFinalCLTVExpiry converts the data (encoded in base32) into a uint64
// to use as the minFinalCLTVExpiry.
func parseMinFinalCLTVExpiry(data []byte) (*uint64, error) {
	expiry, err := base32ToUint64(data)
	if err != nil {
		return nil, err
	}

	return &expiry, nil
}

// parseFallbackAddr converts the data (encoded in base32) into a fallback
// on-chain address.
func parseFallbackAddr(data []byte, net *chaincfg.Params) (btcutil.Address, error) {
	// Checks if the data is empty or contains a version without an address.
	if len(data) < 2 {
		return nil, fmt.Errorf("empty fallback address field")
	}

	var addr btcutil.Address

	version := data[0]
	switch version {
	case 0:
		witness, err := bech32.ConvertBits(data[1:], 5, 8, false)
		if err != nil {
			return nil, err
		}

		switch len(witness) {
		case 20:
			addr, err = btcutil.NewAddressWitnessPubKeyHash(witness, net)
		case 32:
			addr, err = btcutil.NewAddressWitnessScriptHash(witness, net)
		default:
			return nil, fmt.Errorf("unknown witness program length %d",
				len(witness))
		}

		if err != nil {
			return nil, err
		}
	case 17:
		pubKeyHash, err := bech32.ConvertBits(data[1:], 5, 8, false)
		if err != nil {
			return nil, err
		}

		addr, err = btcutil.NewAddressPubKeyHash(pubKeyHash, net)
		if err != nil {
			return nil, err
		}
	case 18:
		scriptHash, err := bech32.ConvertBits(data[1:], 5, 8, false)
		if err != nil {
			return nil, err
		}

		addr, err = btcutil.NewAddressScriptHashFromHash(scriptHash, net)
		if err != nil {
			return nil, err
		}
	default:
		// Ignore unknown version.
	}

	return addr, nil
}

// parseRouteHint converts the data (encoded in base32) into an array containing
// one or more routing hop hints that represent a single route hint.
func parseRouteHint(data []byte) ([]HopHint, error) {
	base256Data, err := bech32.ConvertBits(data, 5, 8, false)
	if err != nil {
		return nil, err
	}

	// Check that base256Data is a multiple of hopHintLen.
	if len(base256Data)%hopHintLen != 0 {
		return nil, fmt.Errorf("expected length multiple of %d bytes, "+
			"got %d", hopHintLen, len(base256Data))
	}

	var routeHint []HopHint

	for len(base256Data) > 0 {
		hopHint := HopHint{}
		hopHint.NodeID, err = btcec.ParsePubKey(base256Data[:33])
		if err != nil {
			return nil, err
		}
		hopHint.ChannelID = binary.BigEndian.Uint64(base256Data[33:41])
		hopHint.FeeBaseMSat = binary.BigEndian.Uint32(base256Data[41:45])
		hopHint.FeeProportionalMillionths = binary.BigEndian.Uint32(base256Data[45:49])
		hopHint.CLTVExpiryDelta = binary.BigEndian.Uint16(base256Data[49:51])

		routeHint = append(routeHint, hopHint)

		base256Data = base256Data[51:]
	}

	return routeHint, nil
}

// parseFeatures decodes any feature bits directly from the base32
// representation.
func parseFeatures(data []byte) (*lnwire.FeatureVector, error) {
	rawFeatures := lnwire.NewRawFeatureVector()
	err := rawFeatures.DecodeBase32(bytes.NewReader(data), len(data))
	if err != nil {
		return nil, err
	}

	return lnwire.NewFeatureVector(rawFeatures, lnwire.Features), nil
}

// base32ToUint64 converts a base32 encoded number to uint64.
func base32ToUint64(data []byte) (uint64, error) {
	// Maximum that fits in uint64 is ceil(64 / 5) = 12 groups.
	if len(data) > 13 {
		return 0, fmt.Errorf("cannot parse data of length %d as uint64",
			len(data))
	}

	val := uint64(0)
	for i := 0; i < len(data); i++ {
		val = val<<5 | uint64(data[i])
	}
	return val, nil
}
