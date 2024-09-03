package netann

import (
	"bytes"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// CreateChanAnnouncement is a helper function which creates all channel
// announcements given the necessary channel related database items. This
// function is used to transform out database structs into the corresponding wire
// structs for announcing new channels to other peers, or simply syncing up a
// peer's initial routing table upon connect.
func CreateChanAnnouncement(chanProof models.ChannelAuthProof,
	chanInfo models.ChannelEdgeInfo,
	edge1, edge2 models.ChannelEdgePolicy) (lnwire.ChannelAnnouncement,
	lnwire.ChannelUpdate, lnwire.ChannelUpdate, error) {

	switch proof := chanProof.(type) {
	case *models.ChannelAuthProof1:
		info, ok := chanInfo.(*models.ChannelEdgeInfo1)
		if !ok {
			return nil, nil, nil, fmt.Errorf("expected type "+
				"ChannelEdgeInfo1 to be paired with "+
				"ChannelAuthProof1, got: %T", chanInfo)
		}

		var e1, e2 *models.ChannelEdgePolicy1
		if edge1 != nil {
			e1, ok = edge1.(*models.ChannelEdgePolicy1)
			if !ok {
				return nil, nil, nil, fmt.Errorf("expected "+
					"type ChannelEdgePolicy1 to be "+
					"paired with ChannelEdgeInfo1, "+
					"got: %T", edge1)
			}
		}

		if edge2 != nil {
			e2, ok = edge2.(*models.ChannelEdgePolicy1)
			if !ok {
				return nil, nil, nil, fmt.Errorf("expected "+
					"type ChannelEdgePolicy1 to be "+
					"paired with ChannelEdgeInfo1, "+
					"got: %T", edge2)
			}
		}

		return createChanAnnouncement1(proof, info, e1, e2)

	case *models.ChannelAuthProof2:
		info, ok := chanInfo.(*models.ChannelEdgeInfo2)
		if !ok {
			return nil, nil, nil, fmt.Errorf("expected type "+
				"ChannelEdgeInfo2 to be paired with "+
				"ChannelAuthProof2, got: %T", chanInfo)
		}

		var e1, e2 *models.ChannelEdgePolicy2
		if edge1 != nil {
			e1, ok = edge1.(*models.ChannelEdgePolicy2)
			if !ok {
				return nil, nil, nil, fmt.Errorf("expected "+
					"type ChannelEdgePolicy2 to be "+
					"paired with ChannelEdgeInfo2, "+
					"got: %T", edge1)
			}
		}

		if edge2 != nil {
			e2, ok = edge2.(*models.ChannelEdgePolicy2)
			if !ok {
				return nil, nil, nil, fmt.Errorf("expected "+
					"type ChannelEdgePolicy2 to be "+
					"paired with ChannelEdgeInfo2, "+
					"got: %T", edge2)
			}
		}

		return createChanAnnouncement2(proof, info, e1, e2)

	default:
		return nil, nil, nil, fmt.Errorf("unhandled "+
			"models.ChannelAuthProof type: %T", chanProof)
	}
}

func createChanAnnouncement1(chanProof *models.ChannelAuthProof1,
	chanInfo *models.ChannelEdgeInfo1,
	e1, e2 *models.ChannelEdgePolicy1) (*lnwire.ChannelAnnouncement1,
	lnwire.ChannelUpdate, lnwire.ChannelUpdate, error) {

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
		Features:        lnwire.NewRawFeatureVector(),
		ExtraOpaqueData: chanInfo.ExtraOpaqueData,
	}

	err := chanAnn.Features.Decode(bytes.NewReader(chanInfo.Features))
	if err != nil {
		return nil, nil, nil, err
	}
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
	var edge1Ann, edge2Ann lnwire.ChannelUpdate
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

func createChanAnnouncement2(chanProof *models.ChannelAuthProof2,
	chanInfo *models.ChannelEdgeInfo2,
	e1, e2 *models.ChannelEdgePolicy2) (lnwire.ChannelAnnouncement,
	lnwire.ChannelUpdate, lnwire.ChannelUpdate, error) {

	// First, using the parameters of the channel, along with the channel
	// authentication chanProof, we'll create re-create the original
	// authenticated channel announcement.
	chanAnn := &lnwire.ChannelAnnouncement2{
		ShortChannelID:  chanInfo.ShortChannelID,
		NodeID1:         chanInfo.NodeID1,
		NodeID2:         chanInfo.NodeID2,
		ChainHash:       chanInfo.ChainHash,
		BitcoinKey1:     chanInfo.BitcoinKey1,
		BitcoinKey2:     chanInfo.BitcoinKey2,
		Features:        chanInfo.Features,
		Capacity:        chanInfo.Capacity,
		ExtraOpaqueData: chanInfo.ExtraOpaqueData,
	}

	var err error
	chanAnn.Signature, err = lnwire.NewSigFromSchnorrRawSignature(
		chanProof.SchnorrSigBytes,
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
	var edge1Ann, edge2Ann lnwire.ChannelUpdate
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
