package brontide

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/hex"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/davecgh/go-spew/spew"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightningnetwork/lnd/keychain"
)

var (
	initBytes = []byte{
		0x81, 0xb6, 0x37, 0xd8, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x96, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe4, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xfd, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	respBytes = []byte{
		0xaa, 0xb6, 0x37, 0xd9, 0xfc, 0xd2, 0xc6, 0xda,
		0x63, 0x59, 0xe6, 0x99, 0x31, 0x13, 0xa1, 0x17,
		0xd, 0xe7, 0x95, 0xe9, 0xb7, 0x25, 0xb8, 0x4d,
		0x1e, 0xb, 0x4c, 0xf9, 0x9e, 0xc5, 0x8c, 0xe9,
	}

	// Returns the initiator's ephemeral private key.
	initEphemeral = EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "121212121212121212121212121212121212121212121212121212" +
			"1212121212"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(eBytes)

		return priv, nil
	})

	// Returns the responder's ephemeral private key.
	respEphemeral = EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "222222222222222222222222222222222222222222222222222" +
			"2222222222222"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(eBytes)

		return priv, nil
	})
)

// completeHandshake takes two brontide machines (initiator, responder)
// and completes the brontide handshake between them. If any part of the
// handshake fails, this function will panic.
func completeHandshake(t *testing.T, initiator, responder *Machine) {
	// Generate ActOne and send to the responder.
	actOne, err := initiator.GenActOne()
	if err != nil {
		dumpAndFail(t, initiator, responder, err)
	}

	if err := responder.RecvActOne(actOne); err != nil {
		dumpAndFail(t, initiator, responder, err)
	}

	// Generate ActTwo and send to initiator.
	actTwo, err := responder.GenActTwo()
	if err != nil {
		dumpAndFail(t, initiator, responder, err)
	}

	if err := initiator.RecvActTwo(actTwo); err != nil {
		dumpAndFail(t, initiator, responder, err)
	}

	// Generate ActThree and send to responder.
	actThree, err := initiator.GenActThree()
	if err != nil {
		dumpAndFail(t, initiator, responder, err)
	}

	if err := responder.RecvActThree(actThree); err != nil {
		dumpAndFail(t, initiator, responder, err)
	}
}

// dumpAndFail dumps the initiator and responder Machines and fails.
func dumpAndFail(t *testing.T, initiator, responder *Machine, err error) {
	t.Helper()

	t.Fatalf("error: %v, initiator: %v, responder: %v", err,
		spew.Sdump(initiator), spew.Sdump(responder))
}

// newInsecurePrivateKey returns a private key that is generated using a
// cryptographically insecure RNG. This function should only be used for testing
// where reproducibility is required.
func newInsecurePrivateKey(t *testing.T,
	insecureRNG io.Reader) *btcec.PrivateKey {

	key, err := ecdsa.GenerateKey(secp256k1.S256(), insecureRNG)
	if err != nil {
		t.Fatalf("error generating private key: %v", err)
	}

	return secp256k1.PrivKeyFromBytes(key.D.Bytes())
}

// getBrontideMachines returns two brontide machines that use pseudorandom keys
// everywhere, generated from seed.
func getBrontideMachines(t *testing.T, seed int64) (*Machine, *Machine) {
	rng := rand.New(rand.NewSource(seed))

	initPriv := newInsecurePrivateKey(t, rng)
	respPriv := newInsecurePrivateKey(t, rng)
	respPub := respPriv.PubKey()

	initPrivECDH := &keychain.PrivKeyECDH{PrivKey: initPriv}
	respPrivECDH := &keychain.PrivKeyECDH{PrivKey: respPriv}

	ephGen := EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		return newInsecurePrivateKey(t, rng), nil
	})

	initiator := NewBrontideMachine(true, initPrivECDH, respPub, ephGen)
	responder := NewBrontideMachine(false, respPrivECDH, nil, ephGen)

	return initiator, responder
}

// getStaticBrontideMachines returns two brontide machines that use static keys
// everywhere.
func getStaticBrontideMachines() (*Machine, *Machine) {
	initPriv, _ := btcec.PrivKeyFromBytes(initBytes)
	respPriv, respPub := btcec.PrivKeyFromBytes(respBytes)

	initPrivECDH := &keychain.PrivKeyECDH{PrivKey: initPriv}
	respPrivECDH := &keychain.PrivKeyECDH{PrivKey: respPriv}

	initiator := NewBrontideMachine(
		true, initPrivECDH, respPub, initEphemeral,
	)
	responder := NewBrontideMachine(
		false, respPrivECDH, nil, respEphemeral,
	)

	return initiator, responder
}

// FuzzRandomActOne fuzz tests ActOne in the brontide handshake.
func FuzzRandomActOne(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Check if data is large enough.
		if len(data) < ActOneSize {
			return
		}

		// This will return brontide machines with random keys.
		_, responder := getBrontideMachines(t, seed)

		// Copy data into [ActOneSize]byte.
		var actOne [ActOneSize]byte
		copy(actOne[:], data)

		// Responder receives ActOne, should fail on the MAC check.
		if err := responder.RecvActOne(actOne); err == nil {
			dumpAndFail(t, nil, responder, nil)
		}
	})
}

// FuzzRandomActThree fuzz tests ActThree in the brontide handshake.
func FuzzRandomActThree(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Check if data is large enough.
		if len(data) < ActThreeSize {
			return
		}

		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Generate ActOne and send to the responder.
		actOne, err := initiator.GenActOne()
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Receiving ActOne should succeed, so we panic on error.
		if err := responder.RecvActOne(actOne); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Generate ActTwo - this is not sent to the initiator because
		// nothing is done with the initiator after this point and it
		// would slow down fuzzing.  GenActTwo needs to be called to set
		// the appropriate state in the responder machine.
		_, err = responder.GenActTwo()
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Copy data into [ActThreeSize]byte.
		var actThree [ActThreeSize]byte
		copy(actThree[:], data)

		// Responder receives ActThree, should fail on the MAC check.
		if err := responder.RecvActThree(actThree); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzRandomActTwo fuzz tests ActTwo in the brontide handshake.
func FuzzRandomActTwo(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Check if data is large enough.
		if len(data) < ActTwoSize {
			return
		}

		// This will return brontide machines with random keys.
		initiator, _ := getBrontideMachines(t, seed)

		// Generate ActOne - this isn't sent to the responder because
		// nothing is done with the responder machine and this would
		// slow down fuzzing.  GenActOne needs to be called to set the
		// appropriate state in the initiator machine.
		_, err := initiator.GenActOne()
		if err != nil {
			dumpAndFail(t, initiator, nil, err)
		}

		// Copy data into [ActTwoSize]byte.
		var actTwo [ActTwoSize]byte
		copy(actTwo[:], data)

		// Initiator receives ActTwo, should fail.
		if err := initiator.RecvActTwo(actTwo); err == nil {
			dumpAndFail(t, initiator, nil, nil)
		}
	})
}

// FuzzRandomInitDecrypt fuzz tests decrypting arbitrary data with the
// initiator.
func FuzzRandomInitDecrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Decrypt the encrypted message using ReadMessage w/ initiator
		// machine.
		if _, err := initiator.ReadMessage(r); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzRandomInitEncDec fuzz tests round-trip encryption and decryption between
// the initiator and the responder.
func FuzzRandomInitEncDec(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ initiator machine.
		if err := initiator.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ initiator machine.
		if _, err := initiator.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Decrypt the ciphertext using ReadMessage w/ responder
		// machine.
		plaintext, err := responder.ReadMessage(&b)
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Check that the decrypted message and the original message are
		// equal.
		if !bytes.Equal(data, plaintext) {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzRandomInitEncrypt fuzz tests the encryption of arbitrary data with the
// initiator.
func FuzzRandomInitEncrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ initiator machine.
		if err := initiator.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ initiator machine.
		if _, err := initiator.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}
	})
}

// FuzzRandomRespDecrypt fuzz tests the decryption of arbitrary data with the
// responder.
func FuzzRandomRespDecrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Decrypt the encrypted message using ReadMessage w/ responder
		// machine.
		if _, err := responder.ReadMessage(r); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzRandomRespEncDec fuzz tests round-trip encryption and decryption between
// the responder and the initiator.
func FuzzRandomRespEncDec(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ responder machine.
		if err := responder.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ responder machine.
		if _, err := responder.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Decrypt the ciphertext using ReadMessage w/ initiator
		// machine.
		plaintext, err := initiator.ReadMessage(&b)
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Check that the decrypted message and the original message are
		// equal.
		if !bytes.Equal(data, plaintext) {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzRandomRespEncrypt fuzz tests encryption of arbitrary data with the
// responder.
func FuzzRandomRespEncrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, seed int64, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with random keys.
		initiator, responder := getBrontideMachines(t, seed)

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ responder machine.
		if err := responder.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ responder machine.
		if _, err := responder.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}
	})
}

// FuzzStaticActOne fuzz tests ActOne in the brontide handshake.
func FuzzStaticActOne(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Check if data is large enough.
		if len(data) < ActOneSize {
			return
		}

		// This will return brontide machines with static keys.
		_, responder := getStaticBrontideMachines()

		// Copy data into [ActOneSize]byte.
		var actOne [ActOneSize]byte
		copy(actOne[:], data)

		// Responder receives ActOne, should fail.
		if err := responder.RecvActOne(actOne); err == nil {
			dumpAndFail(t, nil, responder, nil)
		}
	})
}

// FuzzStaticActThree fuzz tests ActThree in the brontide handshake.
func FuzzStaticActThree(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Check if data is large enough.
		if len(data) < ActThreeSize {
			return
		}

		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Generate ActOne and send to the responder.
		actOne, err := initiator.GenActOne()
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Receiving ActOne should succeed, so we panic on error.
		if err := responder.RecvActOne(actOne); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Generate ActTwo - this is not sent to the initiator because
		// nothing is done with the initiator after this point and it
		// would slow down fuzzing.  GenActTwo needs to be called to set
		// the appropriate state in the responder machine.
		_, err = responder.GenActTwo()
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Copy data into [ActThreeSize]byte.
		var actThree [ActThreeSize]byte
		copy(actThree[:], data)

		// Responder receives ActThree, should fail.
		if err := responder.RecvActThree(actThree); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzStaticActTwo fuzz tests ActTwo in the brontide handshake.
func FuzzStaticActTwo(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Check if data is large enough.
		if len(data) < ActTwoSize {
			return
		}

		// This will return brontide machines with static keys.
		initiator, _ := getStaticBrontideMachines()

		// Generate ActOne - this isn't sent to the responder because
		// nothing is done with the responder machine and this would
		// slow down fuzzing.  GenActOne needs to be called to set the
		// appropriate state in the initiator machine.
		_, err := initiator.GenActOne()
		if err != nil {
			dumpAndFail(t, initiator, nil, err)
		}

		// Copy data into [ActTwoSize]byte.
		var actTwo [ActTwoSize]byte
		copy(actTwo[:], data)

		// Initiator receives ActTwo, should fail.
		if err := initiator.RecvActTwo(actTwo); err == nil {
			dumpAndFail(t, initiator, nil, nil)
		}
	})
}

// FuzzStaticInitDecrypt fuzz tests the decryption of arbitrary data with the
// initiator.
func FuzzStaticInitDecrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Decrypt the encrypted message using ReadMessage w/ initiator
		// machine.
		if _, err := initiator.ReadMessage(r); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzStaticInitEncDec fuzz tests round-trip encryption and decryption between
// the initiator and the responder.
func FuzzStaticInitEncDec(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ initiator machine.
		if err := initiator.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ initiator machine.
		if _, err := initiator.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Decrypt the ciphertext using ReadMessage w/ responder
		// machine.
		plaintext, err := responder.ReadMessage(&b)
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Check that the decrypted message and the original message are
		// equal.
		if !bytes.Equal(data, plaintext) {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzStaticInitEncrypt fuzz tests the encryption of arbitrary data with the
// initiator.
func FuzzStaticInitEncrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ initiator machine.
		if err := initiator.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ initiator machine.
		if _, err := initiator.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}
	})
}

// FuzzStaticRespDecrypt fuzz tests the decryption of arbitrary data with the
// responder.
func FuzzStaticRespDecrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		// Create a reader with the byte array.
		r := bytes.NewReader(data)

		// Decrypt the encrypted message using ReadMessage w/ responder
		// machine.
		if _, err := responder.ReadMessage(r); err == nil {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzStaticRespEncDec fuzz tests the round-trip encryption and decryption
// between the responder and the initiator.
func FuzzStaticRespEncDec(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ responder machine.
		if err := responder.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ responder machine.
		if _, err := responder.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Decrypt the ciphertext using ReadMessage w/ initiator
		// machine.
		plaintext, err := initiator.ReadMessage(&b)
		if err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Check that the decrypted message and the original message are
		// equal.
		if !bytes.Equal(data, plaintext) {
			dumpAndFail(t, initiator, responder, nil)
		}
	})
}

// FuzzStaticRespEncrypt fuzz tests the encryption of arbitrary data with the
// responder.
func FuzzStaticRespEncrypt(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		// Ensure that length of message is not greater than max allowed
		// size.
		if len(data) > math.MaxUint16 {
			return
		}

		// This will return brontide machines with static keys.
		initiator, responder := getStaticBrontideMachines()

		// Complete the brontide handshake.
		completeHandshake(t, initiator, responder)

		var b bytes.Buffer

		// Encrypt the message using WriteMessage w/ responder machine.
		if err := responder.WriteMessage(data); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}

		// Flush the encrypted message w/ responder machine.
		if _, err := responder.Flush(&b); err != nil {
			dumpAndFail(t, initiator, responder, err)
		}
	})
}
