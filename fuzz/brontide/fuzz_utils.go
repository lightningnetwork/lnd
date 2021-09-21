//go:build gofuzz
// +build gofuzz

package brontidefuzz

import (
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcec"
	"github.com/davecgh/go-spew/spew"
	"github.com/lightningnetwork/lnd/brontide"
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
	initEphemeral = brontide.EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "121212121212121212121212121212121212121212121212121212" +
			"1212121212"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(btcec.S256(), eBytes)
		return priv, nil
	})

	// Returns the responder's ephemeral private key.
	respEphemeral = brontide.EphemeralGenerator(func() (*btcec.PrivateKey, error) {
		e := "222222222222222222222222222222222222222222222222222" +
			"2222222222222"
		eBytes, err := hex.DecodeString(e)
		if err != nil {
			return nil, err
		}

		priv, _ := btcec.PrivKeyFromBytes(btcec.S256(), eBytes)
		return priv, nil
	})
)

// completeHandshake takes two brontide machines (initiator, responder)
// and completes the brontide handshake between them. If any part of the
// handshake fails, this function will panic.
func completeHandshake(initiator, responder *brontide.Machine) {
	if err := handshake(initiator, responder); err != nil {
		nilAndPanic(initiator, responder, err)
	}
}

// handshake actually completes the brontide handshake and bubbles up
// an error to the calling function.
func handshake(initiator, responder *brontide.Machine) error {
	// Generate ActOne and send to the responder.
	actOne, err := initiator.GenActOne()
	if err != nil {
		return err
	}

	if err := responder.RecvActOne(actOne); err != nil {
		return err
	}

	// Generate ActTwo and send to initiator.
	actTwo, err := responder.GenActTwo()
	if err != nil {
		return err
	}

	if err := initiator.RecvActTwo(actTwo); err != nil {
		return err
	}

	// Generate ActThree and send to responder.
	actThree, err := initiator.GenActThree()
	if err != nil {
		return err
	}

	return responder.RecvActThree(actThree)
}

// nilAndPanic first nils the initiator and responder's Curve fields and then
// panics.
func nilAndPanic(initiator, responder *brontide.Machine, err error) {
	if initiator != nil {
		initiator.SetCurveToNil()
	}
	if responder != nil {
		responder.SetCurveToNil()
	}
	panic(fmt.Errorf("error: %v, initiator: %v, responder: %v", err,
		spew.Sdump(initiator), spew.Sdump(responder)))
}

// getBrontideMachines returns two brontide machines that use random keys
// everywhere.
func getBrontideMachines() (*brontide.Machine, *brontide.Machine) {
	initPriv, _ := btcec.NewPrivateKey(btcec.S256())
	respPriv, _ := btcec.NewPrivateKey(btcec.S256())
	respPub := (*btcec.PublicKey)(&respPriv.PublicKey)

	initPrivECDH := &keychain.PrivKeyECDH{PrivKey: initPriv}
	respPrivECDH := &keychain.PrivKeyECDH{PrivKey: respPriv}

	initiator := brontide.NewBrontideMachine(true, initPrivECDH, respPub)
	responder := brontide.NewBrontideMachine(false, respPrivECDH, nil)

	return initiator, responder
}

// getStaticBrontideMachines returns two brontide machines that use static keys
// everywhere.
func getStaticBrontideMachines() (*brontide.Machine, *brontide.Machine) {
	initPriv, _ := btcec.PrivKeyFromBytes(btcec.S256(), initBytes)
	respPriv, respPub := btcec.PrivKeyFromBytes(btcec.S256(), respBytes)

	initPrivECDH := &keychain.PrivKeyECDH{PrivKey: initPriv}
	respPrivECDH := &keychain.PrivKeyECDH{PrivKey: respPriv}

	initiator := brontide.NewBrontideMachine(
		true, initPrivECDH, respPub, initEphemeral,
	)
	responder := brontide.NewBrontideMachine(
		false, respPrivECDH, nil, respEphemeral,
	)

	return initiator, responder
}
