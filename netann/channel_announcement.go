package netann

import (
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr/musig2"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/lightningnetwork/lnd/graph/db/models"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tlv"
)

const (
	// chanAnn2MsgName is a string representing the name of the
	// ChannelAnnouncement2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the ChannelAnnouncement2 message.
	chanAnn2MsgName = "channel_announcement_2"

	// chanAnn2SigFieldName is the name of the signature field of the
	// ChannelAnnouncement2 message. This string will be used during the
	// construction of the tagged hash message to be signed when producing
	// the signature for the ChannelAnnouncement2 message.
	chanAnn2SigFieldName = "signature"
)

// CreateChanAnnouncement is a helper function which creates all channel
// announcements given the necessary channel related database items. This
// function is used to transform out database structs into the corresponding wire
// structs for announcing new channels to other peers, or simply syncing up a
// peer's initial routing table upon connect.
func CreateChanAnnouncement(chanProof *models.ChannelAuthProof,
	chanInfo *models.ChannelEdgeInfo,
	e1, e2 *models.ChannelEdgePolicy) (*lnwire.ChannelAnnouncement1,
	*lnwire.ChannelUpdate1, *lnwire.ChannelUpdate1, error) {

	// First, using the parameters of the channel, along with the channel
	// authentication chanProof, we'll create re-create the original
	// authenticated channel announcement.
	chanID := lnwire.NewShortChanIDFromInt(chanInfo.ChannelID)
	chanAnn := &lnwire.ChannelAnnouncement1{
		ShortChannelID:  chanID,
		NodeID1:         chanInfo.NodeKey1Bytes,
		NodeID2:         chanInfo.NodeKey2Bytes,
		ChainHash:       chanInfo.ChainHash,
		BitcoinKey1:     chanInfo.BitcoinKey1Bytes,
		BitcoinKey2:     chanInfo.BitcoinKey2Bytes,
		Features:        chanInfo.Features.RawFeatureVector,
		ExtraOpaqueData: chanInfo.ExtraOpaqueData,
	}

	var err error
	chanAnn.BitcoinSig1, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.BitcoinSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.BitcoinSig2, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.BitcoinSig2Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig1, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.NodeSig1Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}
	chanAnn.NodeSig2, err = lnwire.NewSigFromECDSARawSignature(
		chanProof.NodeSig2Bytes,
	)
	if err != nil {
		return nil, nil, nil, err
	}

	// We'll unconditionally queue the channel's existence chanProof as it
	// will need to be processed before either of the channel update
	// networkMsgs.

	// Since it's up to a node's policy as to whether they advertise the
	// edge in a direction, we don't create an advertisement if the edge is
	// nil.
	var edge1Ann, edge2Ann *lnwire.ChannelUpdate1
	if e1 != nil {
		edge1Ann, err = ChannelUpdateFromEdge(chanInfo, e1)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if e2 != nil {
		edge2Ann, err = ChannelUpdateFromEdge(chanInfo, e2)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return chanAnn, edge1Ann, edge2Ann, nil
}

// FetchPkScript defines a function that can be used to fetch the output script
// for the transaction with the given SCID.
type FetchPkScript func(lnwire.ShortChannelID) ([]byte, error)

// ValidateChannelAnn validates the channel announcement.
func ValidateChannelAnn(a lnwire.ChannelAnnouncement,
	fetchPkScript FetchPkScript) error {

	switch ann := a.(type) {
	case *lnwire.ChannelAnnouncement1:
		return validateChannelAnn1(ann)
	case *lnwire.ChannelAnnouncement2:
		return validateChannelAnn2(ann, fetchPkScript)
	default:
		return fmt.Errorf("unhandled implementation of "+
			"lnwire.ChannelAnnouncement: %T", a)
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

// validateChannelAnn2 validates the channel announcement message and checks
// that message signature covers the announcement message.
func validateChannelAnn2(a *lnwire.ChannelAnnouncement2,
	fetchPkScript FetchPkScript) error {

	dataHash, err := ChanAnn2DigestToSign(a)
	if err != nil {
		return err
	}

	sig, err := a.Signature.Val.ToSignature()
	if err != nil {
		return err
	}

	nodeKey1, err := btcec.ParsePubKey(a.NodeID1.Val[:])
	if err != nil {
		return err
	}

	nodeKey2, err := btcec.ParsePubKey(a.NodeID2.Val[:])
	if err != nil {
		return err
	}

	keys := []*btcec.PublicKey{
		nodeKey1, nodeKey2,
	}

	// If the bitcoin keys are provided in the announcement, then it is
	// assumed that the signature of the announcement is a 4-of-4 MuSig2
	// over the bitcoin keys and node ID keys.
	if a.BitcoinKey1.IsSome() && a.BitcoinKey2.IsSome() {
		var (
			btcKey1 tlv.RecordT[tlv.TlvType12, [33]byte]
			btcKey2 tlv.RecordT[tlv.TlvType14, [33]byte]
		)

		btcKey1 = a.BitcoinKey1.UnwrapOr(btcKey1)
		btcKey2 = a.BitcoinKey2.UnwrapOr(btcKey2)

		bitcoinKey1, err := btcec.ParsePubKey(btcKey1.Val[:])
		if err != nil {
			return err
		}

		bitcoinKey2, err := btcec.ParsePubKey(btcKey2.Val[:])
		if err != nil {
			return err
		}

		keys = append(keys, bitcoinKey1, bitcoinKey2)
	} else {
		// If bitcoin keys are not provided, then we need to get the
		// on-chain output key since this will be the 3rd key in the
		// 3-of-3 MuSig2 signature.
		pkScript, err := fetchPkScript(a.ShortChannelID.Val)
		if err != nil {
			return err
		}

		outputKey, err := schnorr.ParsePubKey(pkScript[2:])
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

// ChanAnn2DigestToSign computes the digest of the message to be signed.
func ChanAnn2DigestToSign(a *lnwire.ChannelAnnouncement2) (*chainhash.Hash,
	error) {

	data, err := lnwire.SerialiseFieldsToSign(a)
	if err != nil {
		return nil, err
	}

	return MsgHash(chanAnn2MsgName, chanAnn2SigFieldName, data), nil
}
