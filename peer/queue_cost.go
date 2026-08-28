package peer

import (
	"net"

	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/tor"
)

const (
	// queuedFeatureOverhead covers the retained feature-vector object and
	// its map allocation independently of the encoded feature-bit span.
	queuedFeatureOverhead = 64

	// queuedFeatureEntryOverhead conservatively covers the map bucket,
	// key, top-hash, and overflow storage retained for each populated bit.
	queuedFeatureEntryOverhead = 16

	// queuedAddrOverhead covers each address interface and decoded object;
	// separately allocated fields are added by nodeAnnouncementAddrCost.
	queuedAddrOverhead = 64

	// queuedShortChanIDSize is the in-memory width of a decoded SCID,
	// including the padding after its uint16 transaction position.
	queuedShortChanIDSize = 12
	// queuedTimestampPairSize covers both retained uint32 update times.
	queuedTimestampPairSize = 8
)

// featureMapCost charges one fixed allocation allowance, the encoded bit span,
// and every populated map entry. Separating span from population keeps sparse
// high bits affordable while preventing dense vectors from hiding map memory.
func featureMapCost(features *lnwire.RawFeatureVector) int {
	if features == nil {
		return 0
	}

	return queuedFeatureOverhead + features.SerializeSize() +
		features.NumFeatures()*queuedFeatureEntryOverhead
}

// nodeAnnouncementAddrCost charges the retained address slice, concrete
// objects, and their separately allocated fields. The type switch mirrors the
// address shapes produced by lnwire decoding without reserializing them.
func nodeAnnouncementAddrCost(addrs []net.Addr) int {
	cost := len(addrs) * queuedAddrOverhead
	for _, addr := range addrs {
		switch addr := addr.(type) {
		case *net.TCPAddr:
			cost += len(addr.IP) + len(addr.Zone)
		case *tor.OnionAddr:
			cost += len(addr.OnionService)
		case *lnwire.DNSAddress:
			cost += len(addr.Hostname)
		case *lnwire.OpaqueAddrs:
			cost += len(addr.Payload)
		}
	}

	return cost
}

// msgCost estimates memory retained by an outgoing message without
// serializing it. The independent count limit still bounds message shapes
// whose dynamic memory is not included in this targeted estimate. The cases
// below enumerate peer-controlled payloads that can arrive in bulk; CommitSig
// signatures are the known material undercount, but commitment flow control
// bounds them by channel count rather than permitting a bulk peer flood.
func (l queueLimits) msgCost(msg lnwire.Message) int {
	switch msg := msg.(type) {
	// Pong payloads alias one server-wide buffer, so only their wrapper and
	// list storage contribute additional retained queue memory.
	case *lnwire.Pong:
		return l.msgOverhead

	// Failure reasons are preserved byte-for-byte when forwarded upstream
	// and are the largest variable payload a remote peer can drive in bulk.
	case *lnwire.UpdateFailHTLC:
		return l.msgOverhead + len(msg.Reason)

	// The onion packet is inline rather than a slice, so charge it together
	// with any separately retained extra data.
	case *lnwire.UpdateAddHTLC:
		return l.msgOverhead + lnwire.OnionPacketSize +
			len(msg.ExtraData)

	// Error and Warning retain peer-controlled diagnostic payloads, so
	// charge their backing bytes against the queue memory limit.
	case *lnwire.Error:
		return l.msgOverhead + len(msg.Data)

	case *lnwire.Warning:
		return l.msgOverhead + len(msg.Data)

	// V1 gossip forwarding preserves peer-authored opaque extensions in
	// each queued message. Feature maps and node addresses are decoded into
	// larger retained objects, so charge those allocations as well.
	case *lnwire.ChannelAnnouncement1:
		return l.msgOverhead + len(msg.ExtraOpaqueData) +
			featureMapCost(msg.Features)

	case *lnwire.NodeAnnouncement1:
		return l.msgOverhead + len(msg.ExtraOpaqueData) +
			featureMapCost(msg.Features) +
			nodeAnnouncementAddrCost(msg.Addresses)

	case *lnwire.ChannelUpdate1:
		return l.msgOverhead + len(msg.ExtraOpaqueData)

	// Gossip queries retain decoded slices that can be much larger than
	// their compressed wire forms, along with any unknown TLV bytes.
	case *lnwire.QueryChannelRange:
		cost := l.msgOverhead + len(msg.ExtraData)
		if msg.QueryOptions != nil {
			features := lnwire.RawFeatureVector(*msg.QueryOptions)
			cost += featureMapCost(&features)
		}

		return cost

	case *lnwire.QueryShortChanIDs:
		return l.msgOverhead +
			len(msg.ShortChanIDs)*queuedShortChanIDSize +
			len(msg.ExtraData)

	case *lnwire.ReplyChannelRange:
		return l.msgOverhead +
			len(msg.ShortChanIDs)*queuedShortChanIDSize +
			len(msg.Timestamps)*queuedTimestampPairSize +
			len(msg.ExtraData)

	// Other messages receive the fixed charge. Their total count is still
	// bounded even if they retain dynamic data not enumerated above.
	default:
		return l.msgOverhead
	}
}
