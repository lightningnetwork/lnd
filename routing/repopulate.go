package routing

import (
	"bytes"
	"fmt"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channeldb/models"
	"github.com/lightningnetwork/lnd/lnwire"
)

// EdgeRepopulator's only job is to ensure every local channel has a
// corresponding edge and policy in the graph database.
type EdgeRepopulator struct {
	LocalPubkey          btcec.PublicKey
	DefaultRoutingPolicy models.ForwardingPolicy
	ChanStateDB          *channeldb.ChannelStateDB
	GraphDB              *channeldb.ChannelGraph
}

// RepopulateMissingEdges iterates all open channels and checks whether a local
// channel policy is stored for that channel. If the policy is missing, it is
// populated with the default channel policy.
func (e *EdgeRepopulator) RepopulateMissingEdges() error {
	log.Debug("RepopulateMissingEdges started")

	// First fetch all current open channels.
	channels, err := e.ChanStateDB.FetchAllOpenChannels()
	if err != nil {
		return fmt.Errorf("failed to fetch open channels: %w", err)
	}

	for _, c := range channels {
		// For every channel, check the graph whether there is a
		// corresponding edge.
		edge1Timestamp, edge2Timestamp, exists, _, err :=
			e.GraphDB.HasChannelEdge(c.ShortChanID().ToUint64())
		if err != nil {
			log.Warnf("Failed to populate edge for channel %s: %v",
				c.FundingOutpoint.String(), err)
			continue
		}

		// Initialize some fields with relatively long lines here.
		localBytes := e.LocalPubkey.SerializeCompressed()
		remoteBytes := c.IdentityPub.SerializeCompressed()
		localBitcoin := c.LocalChanCfg.MultiSigKey.PubKey.
			SerializeCompressed()
		remoteBitcoin := c.RemoteChanCfg.MultiSigKey.PubKey.
			SerializeCompressed()

		// local1 is a value indicating whether the local node is node 1
		// in the channel edge. If true, the local node is node 1, if
		// false the local node is node 2.
		local1 := bytes.Compare(localBytes, remoteBytes) < 0

		// If the channel edge belonging to the current channel doesn't
		// exist in the graph, it is added here.
		if !exists {
			var (
				nodeKey1Bytes, nodeKey2Bytes,
				bitcoinKey1Bytes, bitcoinKey2Bytes []byte
				featureBuf bytes.Buffer
			)

			if local1 {
				nodeKey1Bytes = localBytes
				nodeKey2Bytes = remoteBytes
				bitcoinKey1Bytes = localBitcoin
				bitcoinKey2Bytes = remoteBitcoin
			} else {
				nodeKey1Bytes = remoteBytes
				nodeKey2Bytes = localBytes
				bitcoinKey1Bytes = remoteBitcoin
				bitcoinKey2Bytes = localBitcoin
			}

			err = lnwire.NewRawFeatureVector().Encode(&featureBuf)
			if err != nil {
				return fmt.Errorf(
					"unable to encode features: %v", err)
			}

			edge := &models.ChannelEdgeInfo{
				ChannelID:    c.ShortChanID().ToUint64(),
				ChainHash:    c.ChainHash,
				Features:     featureBuf.Bytes(),
				Capacity:     c.Capacity,
				ChannelPoint: c.FundingOutpoint,
			}

			copy(edge.NodeKey1Bytes[:], nodeKey1Bytes)
			copy(edge.NodeKey2Bytes[:], nodeKey2Bytes)
			copy(edge.BitcoinKey1Bytes[:], bitcoinKey1Bytes)
			copy(edge.BitcoinKey2Bytes[:], bitcoinKey2Bytes)

			// Add the newly created, missing edge to the graph.
			err := e.GraphDB.AddChannelEdge(edge)
			if err != nil {
				log.Errorf("Failed to fix missing edge for "+
					"local channel %s: %w",
					c.FundingOutpoint.String(), err)
				continue
			}

			log.Infof("Fixed missing edge for local channel %s",
				c.FundingOutpoint.String())
		}

		var localTimestamp time.Time
		var chanFlags lnwire.ChanUpdateChanFlags
		if local1 {
			localTimestamp = edge1Timestamp
			chanFlags = 0
		} else {
			localTimestamp = edge2Timestamp
			chanFlags = 1
		}

		policy := e.DefaultRoutingPolicy
		msgFlags := lnwire.ChanUpdateRequiredMaxHtlc
		localPolicy := &models.ChannelEdgePolicy{
			ChannelID:                 c.ShortChanID().ToUint64(),
			LastUpdate:                time.Now(),
			MessageFlags:              msgFlags,
			ChannelFlags:              chanFlags,
			TimeLockDelta:             uint16(policy.TimeLockDelta),
			MinHTLC:                   policy.MinHTLCOut,
			MaxHTLC:                   policy.MaxHTLC,
			FeeBaseMSat:               policy.BaseFee,
			FeeProportionalMillionths: policy.FeeRate,
		}

		// If the update timestamp of the local edge policy is zero,
		// that means the local channel policy is missing. It is added
		// here.
		if localTimestamp.IsZero() {
			err := e.GraphDB.UpdateEdgePolicy(localPolicy)
			if err != nil {
				log.Errorf("Failed to fix missing policy for "+
					"local channel %s: %w",
					c.FundingOutpoint.String(), err)
				continue
			}

			log.Infof("Fixed missing policy for local channel %s",
				c.FundingOutpoint.String())
		}
	}

	log.Debug("RepopulateMissingEdges completed")
	return nil
}
