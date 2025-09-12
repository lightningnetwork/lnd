package zpay32

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/lnwire"
)

// Encode takes the given MessageSigner and returns a string encoding this
// invoice signed by the node key of the signer.
func (invoice *Invoice) Encode(signer MessageSigner) (string, error) {
	// First check that this invoice is valid before starting the encoding.
	if err := validateInvoice(invoice); err != nil {
		return "", err
	}

	// The buffer will encoded the invoice data using 5-bit groups (base32).
	var bufferBase32 bytes.Buffer

	// The timestamp will be encoded using 35 bits, in base32.
	timestampBase32 := uint64ToBase32(uint64(invoice.Timestamp.Unix()))

	// The timestamp must be exactly 35 bits, which means 7 groups. If it
	// can fit into fewer groups we add leading zero groups, if it is too
	// big we fail early, as there is not possible to encode it.
	if len(timestampBase32) > timestampBase32Len {
		return "", fmt.Errorf("timestamp too big: %d",
			invoice.Timestamp.Unix())
	}

	// Add zero bytes to the first timestampBase32Len-len(timestampBase32)
	// groups, then add the non-zero groups.
	zeroes := make([]byte, timestampBase32Len-len(timestampBase32))
	_, err := bufferBase32.Write(zeroes)
	if err != nil {
		return "", fmt.Errorf("unable to write to buffer: %w", err)
	}
	_, err = bufferBase32.Write(timestampBase32)
	if err != nil {
		return "", fmt.Errorf("unable to write to buffer: %w", err)
	}

	// We now write the tagged fields to the buffer, which will fill the
	// rest of the data part before the signature.
	if err := writeTaggedFields(&bufferBase32, invoice); err != nil {
		return "", err
	}

	// The human-readable part (hrp) is "ln" + net hrp + optional amount,
	// except for signet where we add an additional "s" to differentiate it
	// from the older testnet3 (Core devs decided to use the same hrp for
	// signet as for testnet3 which is not optimal for LN). See
	// https://github.com/lightningnetwork/lightning-rfc/pull/844 for more
	// information.
	hrp := "ln" + invoice.Net.Bech32HRPSegwit
	if invoice.Net.Name == chaincfg.SigNetParams.Name {
		hrp = "lntbs"
	}
	if invoice.MilliSat != nil {
		// Encode the amount using the fewest possible characters.
		am, err := encodeAmount(*invoice.MilliSat)
		if err != nil {
			return "", err
		}
		hrp += am
	}

	// The signature is over the single SHA-256 hash of the hrp + the
	// tagged fields encoded in base256.
	taggedFieldsBytes, err := bech32.ConvertBits(bufferBase32.Bytes(), 5, 8, true)
	if err != nil {
		return "", err
	}

	toSign := append([]byte(hrp), taggedFieldsBytes...)

	// We use compact signature format, and also encoded the recovery ID
	// such that a reader of the invoice can recover our pubkey from the
	// signature.
	sign, err := signer.SignCompact(toSign)
	if err != nil {
		return "", err
	}

	// From the header byte we can extract the recovery ID, and the last 64
	// bytes encode the signature.
	recoveryID := sign[0] - 27 - 4
	sig, err := lnwire.NewSigFromWireECDSA(sign[1:])
	if err != nil {
		return "", err
	}

	// If the pubkey field was explicitly set, it must be set to the pubkey
	// used to create the signature.
	if invoice.Destination != nil {
		signature, err := sig.ToSignature()
		if err != nil {
			return "", fmt.Errorf("unable to deserialize "+
				"signature: %v", err)
		}

		hash := chainhash.HashB(toSign)
		valid := signature.Verify(hash, invoice.Destination)
		if !valid {
			return "", fmt.Errorf("signature does not match " +
				"provided pubkey")
		}
	}

	// Convert the signature to base32 before writing it to the buffer.
	signBase32, err := bech32.ConvertBits(
		append(sig.RawBytes(), recoveryID),
		8, 5, true,
	)
	if err != nil {
		return "", err
	}
	bufferBase32.Write(signBase32)

	// Now we can create the bech32 encoded string from the base32 buffer.
	b32, err := bech32.Encode(hrp, bufferBase32.Bytes())
	if err != nil {
		return "", err
	}

	// Before returning, check that the bech32 encoded string is not greater
	// than our largest supported invoice size.
	if len(b32) > maxInvoiceLength {
		return "", ErrInvoiceTooLarge
	}

	return b32, nil
}

// writeTaggedFields writes the non-nil tagged fields of the Invoice to the
// base32 buffer.
func writeTaggedFields(bufferBase32 *bytes.Buffer, invoice *Invoice) error {
	if invoice.PaymentHash != nil {
		err := writeBytes32(bufferBase32, fieldTypeP, *invoice.PaymentHash)
		if err != nil {
			return err
		}
	}

	if invoice.Description != nil {
		base32, err := bech32.ConvertBits([]byte(*invoice.Description),
			8, 5, true)
		if err != nil {
			return err
		}
		err = writeTaggedField(bufferBase32, fieldTypeD, base32)
		if err != nil {
			return err
		}
	}

	if invoice.DescriptionHash != nil {
		err := writeBytes32(
			bufferBase32, fieldTypeH, *invoice.DescriptionHash,
		)
		if err != nil {
			return err
		}
	}

	if invoice.Metadata != nil {
		base32, err := bech32.ConvertBits(invoice.Metadata, 8, 5, true)
		if err != nil {
			return err
		}
		err = writeTaggedField(bufferBase32, fieldTypeM, base32)
		if err != nil {
			return err
		}
	}

	if invoice.minFinalCLTVExpiry != nil {
		finalDelta := uint64ToBase32(*invoice.minFinalCLTVExpiry)
		err := writeTaggedField(bufferBase32, fieldTypeC, finalDelta)
		if err != nil {
			return err
		}
	}

	if invoice.expiry != nil {
		seconds := invoice.expiry.Seconds()
		expiry := uint64ToBase32(uint64(seconds))
		err := writeTaggedField(bufferBase32, fieldTypeX, expiry)
		if err != nil {
			return err
		}
	}

	if invoice.FallbackAddr != nil {
		var version byte
		switch addr := invoice.FallbackAddr.(type) {
		case *btcutil.AddressPubKeyHash:
			version = fallbackVersionPubkeyHash
		case *btcutil.AddressScriptHash:
			version = fallbackVersionScriptHash
		case *btcutil.AddressWitnessPubKeyHash:
			version = addr.WitnessVersion()
		case *btcutil.AddressWitnessScriptHash:
			version = addr.WitnessVersion()
		case *btcutil.AddressTaproot:
			version = addr.WitnessVersion()
		default:
			return fmt.Errorf("unknown fallback address type")
		}
		base32Addr, err := bech32.ConvertBits(
			invoice.FallbackAddr.ScriptAddress(), 8, 5, true)
		if err != nil {
			return err
		}

		err = writeTaggedField(bufferBase32, fieldTypeF,
			append([]byte{version}, base32Addr...))
		if err != nil {
			return err
		}
	}

	for _, routeHint := range invoice.RouteHints {
		// Each hop hint is encoded using 51 bytes, so we'll make to
		// sure to allocate enough space for the whole route hint.
		routeHintBase256 := make([]byte, 0, hopHintLen*len(routeHint))

		for _, hopHint := range routeHint {
			hopHintBase256 := make([]byte, hopHintLen)
			copy(hopHintBase256[:33], hopHint.NodeID.SerializeCompressed())
			binary.BigEndian.PutUint64(
				hopHintBase256[33:41], hopHint.ChannelID,
			)
			binary.BigEndian.PutUint32(
				hopHintBase256[41:45], hopHint.FeeBaseMSat,
			)
			binary.BigEndian.PutUint32(
				hopHintBase256[45:49], hopHint.FeeProportionalMillionths,
			)
			binary.BigEndian.PutUint16(
				hopHintBase256[49:51], hopHint.CLTVExpiryDelta,
			)
			routeHintBase256 = append(routeHintBase256, hopHintBase256...)
		}

		routeHintBase32, err := bech32.ConvertBits(
			routeHintBase256, 8, 5, true,
		)
		if err != nil {
			return err
		}

		err = writeTaggedField(bufferBase32, fieldTypeR, routeHintBase32)
		if err != nil {
			return err
		}
	}

	for _, path := range invoice.BlindedPaymentPaths {
		var buf bytes.Buffer

		err := path.Encode(&buf)
		if err != nil {
			return err
		}

		blindedPathBase32, err := bech32.ConvertBits(
			buf.Bytes(), 8, 5, true,
		)
		if err != nil {
			return err
		}

		err = writeTaggedField(
			bufferBase32, fieldTypeB, blindedPathBase32,
		)
		if err != nil {
			return err
		}
	}

	if invoice.Destination != nil {
		// Convert 33 byte pubkey to 53 5-bit groups.
		pubKeyBase32, err := bech32.ConvertBits(
			invoice.Destination.SerializeCompressed(), 8, 5, true)
		if err != nil {
			return err
		}

		if len(pubKeyBase32) != pubKeyBase32Len {
			return fmt.Errorf("invalid pubkey length: %d",
				len(invoice.Destination.SerializeCompressed()))
		}

		err = writeTaggedField(bufferBase32, fieldTypeN, pubKeyBase32)
		if err != nil {
			return err
		}
	}

	err := fn.MapOptionZ(invoice.PaymentAddr, func(addr [32]byte) error {
		return writeBytes32(bufferBase32, fieldTypeS, addr)
	})
	if err != nil {
		return err
	}

	if invoice.Features.SerializeSize32() > 0 {
		var b bytes.Buffer
		err := invoice.Features.RawFeatureVector.EncodeBase32(&b)
		if err != nil {
			return err
		}

		err = writeTaggedField(bufferBase32, fieldType9, b.Bytes())
		if err != nil {
			return err
		}
	}

	return nil
}

// writeBytes32 encodes a 32-byte array as base32 and writes it to bufferBase32
// under the passed fieldType.
func writeBytes32(bufferBase32 *bytes.Buffer, fieldType byte, b [32]byte) error {
	// Convert 32 byte hash to 52 5-bit groups.
	base32, err := bech32.ConvertBits(b[:], 8, 5, true)
	if err != nil {
		return err
	}

	return writeTaggedField(bufferBase32, fieldType, base32)
}

// writeTaggedField takes the type of a tagged data field, and the data of
// the tagged field (encoded in base32), and writes the type, length and data
// to the buffer.
func writeTaggedField(bufferBase32 *bytes.Buffer, dataType byte, data []byte) error {
	// Length must be exactly 10 bits, so add leading zero groups if
	// needed.
	lenBase32 := uint64ToBase32(uint64(len(data)))
	for len(lenBase32) < 2 {
		lenBase32 = append([]byte{0}, lenBase32...)
	}

	if len(lenBase32) != 2 {
		return fmt.Errorf("data length too big to fit within 10 bits: %d",
			len(data))
	}

	err := bufferBase32.WriteByte(dataType)
	if err != nil {
		return fmt.Errorf("unable to write to buffer: %w", err)
	}
	_, err = bufferBase32.Write(lenBase32)
	if err != nil {
		return fmt.Errorf("unable to write to buffer: %w", err)
	}
	_, err = bufferBase32.Write(data)
	if err != nil {
		return fmt.Errorf("unable to write to buffer: %w", err)
	}

	return nil
}

// uint64ToBase32 converts a uint64 to a base32 encoded integer encoded using
// as few 5-bit groups as possible.
func uint64ToBase32(num uint64) []byte {
	// Return at least one group.
	if num == 0 {
		return []byte{0}
	}

	// To fit an uint64, we need at most is ceil(64 / 5) = 13 groups.
	arr := make([]byte, 13)
	i := 13
	for num > 0 {
		i--
		arr[i] = byte(num & uint64(31)) // 0b11111 in binary
		num >>= 5
	}

	// We only return non-zero leading groups.
	return arr[i:]
}
