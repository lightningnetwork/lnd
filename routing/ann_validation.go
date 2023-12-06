package routing

import (
	"bytes"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/davecgh/go-spew/spew"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/lnwallet/chanvalidate"
	"github.com/lightningnetwork/lnd/lnwire"
)

// ValidateChannelAnn validates the signature(s) of a channel announcement
// message.
func ValidateChannelAnn(a lnwire.ChannelAnnouncement,
	fetchTx func(id *lnwire.ShortChannelID) (*wire.MsgTx, error)) error {

	switch ann := a.(type) {
	case *lnwire.ChannelAnnouncement1:
		return validateChannelAnn1(ann)
	case *lnwire.ChannelAnnouncement2:
		return validateChannelAnn2(ann, fetchTx)
	default:
		return fmt.Errorf("unhandled lnwire.ChannelAnnouncement "+
			"implementation: %T", ann)
	}
}

// validateChannelAnn1 validates the channel announcement message and checks
// that node signatures covers the announcement message, and that the bitcoin
// signatures covers the node keys.
func validateChannelAnn1(a *lnwire.ChannelAnnouncement1) error {
	// First, we'll compute the digest (h) which is to be signed by each of
	// the keys included within the node announcement message. This hash
	// digest includes all the keys, so the (up to 4 signatures) will
	// attest to the validity of each of the keys.
	data, err := a.DataToSign()
	if err != nil {
		return err
	}
	dataHash := chainhash.DoubleHashB(data)

	// First we'll verify that the passed bitcoin key signature is indeed a
	// signature over the computed hash digest.
	bitcoinSig1, err := a.BitcoinSig1.ToSignature()
	if err != nil {
		return err
	}
	bitcoinKey1, err := btcec.ParsePubKey(a.BitcoinKey1[:])
	if err != nil {
		return err
	}
	if !bitcoinSig1.Verify(dataHash, bitcoinKey1) {
		return errors.New("can't verify first bitcoin signature")
	}

	// If that checks out, then we'll verify that the second bitcoin
	// signature is a valid signature of the bitcoin public key over hash
	// digest as well.
	bitcoinSig2, err := a.BitcoinSig2.ToSignature()
	if err != nil {
		return err
	}
	bitcoinKey2, err := btcec.ParsePubKey(a.BitcoinKey2[:])
	if err != nil {
		return err
	}
	if !bitcoinSig2.Verify(dataHash, bitcoinKey2) {
		return errors.New("can't verify second bitcoin signature")
	}

	// Both node signatures attached should indeed be a valid signature
	// over the selected digest of the channel announcement signature.
	nodeSig1, err := a.NodeSig1.ToSignature()
	if err != nil {
		return err
	}
	nodeKey1, err := btcec.ParsePubKey(a.NodeID1[:])
	if err != nil {
		return err
	}
	if !nodeSig1.Verify(dataHash, nodeKey1) {
		return errors.New("can't verify data in first node signature")
	}

	nodeSig2, err := a.NodeSig2.ToSignature()
	if err != nil {
		return err
	}
	nodeKey2, err := btcec.ParsePubKey(a.NodeID2[:])
	if err != nil {
		return err
	}
	if !nodeSig2.Verify(dataHash, nodeKey2) {
		return errors.New("can't verify data in second node signature")
	}

	return nil

}

// validateChannelAnn2 validates the channel announcement 2 message and checks
// that schnorr signature is valid.
func validateChannelAnn2(a *lnwire.ChannelAnnouncement2,
	fetchTx func(id *lnwire.ShortChannelID) (*wire.MsgTx, error)) error {

	dataHash, err := a.DigestToSign()
	if err != nil {
		return err
	}

	sig, err := a.Signature.ToSignature()
	if err != nil {
		return err
	}

	nodeKey1, err := btcec.ParsePubKey(a.NodeID1[:])
	if err != nil {
		return err
	}

	nodeKey2, err := btcec.ParsePubKey(a.NodeID2[:])
	if err != nil {
		return err
	}

	keys := []*btcec.PublicKey{
		nodeKey1, nodeKey2,
	}

	var (
		btcKey1 [33]byte
		btcKey2 [33]byte
	)
	// If the bitcoin keys are provided in the announcement, then it is
	// assumed that the signature of the announcement is a 4-of-4 MuSig2
	// over the bitcoin keys and node ID keys
	if a.BitcoinKey1.IsSome() && a.BitcoinKey2.IsSome() {
		btcKey1 = a.BitcoinKey1.UnwrapOr(btcKey1)
		btcKey2 = a.BitcoinKey2.UnwrapOr(btcKey2)

		bitcoinKey1, err := btcec.ParsePubKey(btcKey1[:])
		if err != nil {
			return err
		}

		bitcoinKey2, err := btcec.ParsePubKey(btcKey2[:])
		if err != nil {
			return err
		}

		keys = append(keys, bitcoinKey1, bitcoinKey2)

	} else {
		// TODO(elle): ensure that this is covered by a test.

		// If bitcoin keys are not provided, then we need to get the
		// on-chain output key since this will be the 3rd key in the
		// 3-of-3 MuSig2 signature.
		tx, err := fetchTx(&a.ShortChannelID)
		if err != nil {
			return err
		}

		outputLocator := chanvalidate.ShortChanIDChanLocator{
			ID: a.ShortChannelID,
		}

		output, _, err := outputLocator.Locate(tx)
		if err != nil {
			return err
		}

		// Taproot PkScripts should be 33 bytes: OP_1 + 32-byte key.
		pkScript := output.PkScript
		if len(pkScript) != 33 || pkScript[0] != txscript.OP_1 {
			return fmt.Errorf("not a taproot output")
		}

		outputKey, err := schnorr.ParsePubKey(pkScript[1:])
		if err != nil {
			return err
		}

		keys = append(keys, outputKey)
	}

	aggKey, _, _, err := musig2.AggregateKeys(keys, true)
	if err != nil {
		return err
	}

	if !sig.Verify(dataHash.CloneBytes(), aggKey.FinalKey) {
		return fmt.Errorf("invalid sig")
	}

	return nil
}

// ValidateNodeAnn validates the node announcement by ensuring that the
// attached signature is needed a signature of the node announcement under the
// specified node public key.
func ValidateNodeAnn(a *lnwire.NodeAnnouncement1) error {
	// Reconstruct the data of announcement which should be covered by the
	// signature so we can verify the signature shortly below
	data, err := a.DataToSign()
	if err != nil {
		return err
	}

	nodeSig, err := a.Signature.ToSignature()
	if err != nil {
		return err
	}
	nodeKey, err := btcec.ParsePubKey(a.NodeID[:])
	if err != nil {
		return err
	}

	// Finally ensure that the passed signature is valid, if not we'll
	// return an error so this node announcement can be rejected.
	dataHash := chainhash.DoubleHashB(data)
	if !nodeSig.Verify(dataHash, nodeKey) {
		var msgBuf bytes.Buffer
		if _, err := lnwire.WriteMessage(&msgBuf, a, 0); err != nil {
			return err
		}

		return errors.Errorf("signature on NodeAnnouncement1(%x) is "+
			"invalid: %x", nodeKey.SerializeCompressed(),
			msgBuf.Bytes())
	}

	return nil
}

// ValidateChannelUpdateAnn validates the channel update announcement by
// checking (1) that the included signature covers the announcement and has been
// signed by the node's private key, and (2) that the announcement's message
// flags and optional fields are sane.
func ValidateChannelUpdateAnn(pubKey *btcec.PublicKey, capacity btcutil.Amount,
	a *lnwire.ChannelUpdate1) error {

	if err := ValidateChannelUpdateFields(capacity, a); err != nil {
		return err
	}

	return VerifyChannelUpdateSignature(a, pubKey)
}

// VerifyChannelUpdateSignature verifies that the channel update message was
// signed by the party with the given node public key.
func VerifyChannelUpdateSignature(msg *lnwire.ChannelUpdate1,
	pubKey *btcec.PublicKey) error {

	data, err := msg.DataToSign()
	if err != nil {
		return fmt.Errorf("unable to reconstruct message data: %v", err)
	}
	dataHash := chainhash.DoubleHashB(data)

	nodeSig, err := msg.Signature.ToSignature()
	if err != nil {
		return err
	}

	if !nodeSig.Verify(dataHash, pubKey) {
		return fmt.Errorf("invalid signature for channel update %v",
			spew.Sdump(msg))
	}

	return nil
}

// ValidateChannelUpdateFields validates a channel update's message flags and
// corresponding update fields.
func ValidateChannelUpdateFields(capacity btcutil.Amount,
	msg lnwire.ChannelUpdate) error {

	switch m := msg.(type) {
	case *lnwire.ChannelUpdate1:
		return validateChannelUpdate1Fields(capacity, m)

	case *lnwire.ChannelUpdate2:
		return validateChannelUpdate2Fields(capacity, m)

	default:
		return fmt.Errorf("unhandled implementation of "+
			"lnwire.ChannelUpdate: %T", msg)
	}

	return nil
}

func validateChannelUpdate1Fields(capacity btcutil.Amount,
	msg *lnwire.ChannelUpdate1) error {

	// The maxHTLC flag is mandatory.
	if !msg.MessageFlags.HasMaxHtlc() {
		return errors.Errorf("max htlc flag not set for "+
			"channel update %v", spew.Sdump(msg))
	}

	maxHtlc := msg.HtlcMaximumMsat
	if maxHtlc == 0 || maxHtlc < msg.HtlcMinimumMsat {
		return errors.Errorf("invalid max htlc for channel "+
			"update %v", spew.Sdump(msg))
	}

	// For light clients, the capacity will not be set so we'll skip
	// checking whether the MaxHTLC value respects the channel's
	// capacity.
	capacityMsat := lnwire.NewMSatFromSatoshis(capacity)
	if capacityMsat != 0 && maxHtlc > capacityMsat {
		return errors.Errorf("max_htlc (%v) for channel "+
			"update greater than capacity (%v)", maxHtlc,
			capacityMsat)
	}

	return nil
}

func validateChannelUpdate2Fields(capacity btcutil.Amount,
	msg *lnwire.ChannelUpdate2) error {

	maxHtlc := msg.HTLCMaximumMsat
	if maxHtlc == 0 || maxHtlc < msg.HTLCMinimumMsat {
		return errors.Errorf("invalid max htlc for channel "+
			"update %v", spew.Sdump(msg))
	}

	// Checking whether the MaxHTLC value respects the channel's
	// capacity.
	capacityMsat := lnwire.NewMSatFromSatoshis(capacity)
	if maxHtlc > capacityMsat {
		return errors.Errorf("max_htlc (%v) for channel "+
			"update greater than capacity (%v)", maxHtlc,
			capacityMsat)
	}

	return nil
}
