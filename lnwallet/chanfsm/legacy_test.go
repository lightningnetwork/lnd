package chanfsm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chainfee"
	"github.com/lightningnetwork/lnd/lnwire"
)

// This file holds the legacy driver: a node that runs the commitment protocol
// by calling a LightningChannel directly, in the order htlcswitch's link
// does. It is the old implementation the differential tests compare the
// state machine against, and the honest counterparty in every test.

// htlcAmt is the amount of every HTLC the tests add.
const htlcAmt = lnwire.MilliSatoshi(10_000_000)

// preimageFor returns the preimage of the HTLC with the given ID offered by
// the named node. Both worlds of a differential test derive the same one.
func preimageFor(owner string, id uint64) [32]byte {
	var buf [40]byte
	copy(buf[:], owner)
	binary.BigEndian.PutUint64(buf[32:], id)

	return sha256.Sum256(buf[:])
}

// newAdd returns the update_add_htlc a node sends for its HTLC with the given
// ID.
func newAdd(chanID lnwire.ChannelID, owner string,
	id uint64) *lnwire.UpdateAddHTLC {

	preimage := preimageFor(owner, id)

	return &lnwire.UpdateAddHTLC{
		ChanID:      chanID,
		ID:          id,
		Amount:      htlcAmt,
		PaymentHash: sha256.Sum256(preimage[:]),
		Expiry:      500_000,
	}
}

// legacyNode drives a LightningChannel the way the link does.
type legacyNode struct {
	// name identifies the node, and seeds its preimages.
	name string

	// peer is the counterparty's name, whose preimages we learn.
	peer string

	// lc is the channel.
	lc *lnwallet.LightningChannel

	// sent collects the messages the node sends, in order.
	sent []lnwire.Message

	// fwds collects the forwarding packages the node produces.
	fwds []*channeldb.FwdPkg

	// failed is set once the node fails the channel.
	failed error
}

// send queues a message to the peer.
func (n *legacyNode) send(msgs ...lnwire.Message) {
	n.sent = append(n.sent, msgs...)
}

// fail records a channel failure and returns it.
func (n *legacyNode) fail(err error) error {
	n.failed = err
	return err
}

// addHTLC offers the next HTLC.
func (n *legacyNode) addHTLC() (uint64, error) {
	id := n.nextHtlcID()
	add := newAdd(n.lc.ChannelID(), n.name, id)
	got, err := n.lc.AddHTLC(add, nil)
	if err != nil {
		return 0, err
	}
	if got != id {
		return 0, fmt.Errorf("AddHTLC took ID %d, want %d", got, id)
	}
	n.send(add)

	return id, nil
}

// nextHtlcID returns the ID our next HTLC takes.
func (n *legacyNode) nextHtlcID() uint64 {
	return n.lc.ProtocolSnapshot().Logs.Local.HtlcCounter
}

// settleHTLC settles the peer's HTLC with the given ID.
func (n *legacyNode) settleHTLC(id uint64) error {
	preimage := preimageFor(n.peer, id)
	err := n.lc.SettleHTLC(preimage, id, nil, nil, nil)
	if err != nil {
		return err
	}
	n.send(&lnwire.UpdateFulfillHTLC{
		ChanID:          n.lc.ChannelID(),
		ID:              id,
		PaymentPreimage: preimage,
	})

	return nil
}

// failHTLC fails the peer's HTLC with the given ID.
func (n *legacyNode) failHTLC(id uint64) error {
	reason := []byte("fail")
	err := n.lc.FailHTLC(id, reason, nil, nil, nil)
	if err != nil {
		return err
	}
	n.send(&lnwire.UpdateFailHTLC{
		ChanID: n.lc.ChannelID(),
		ID:     id,
		Reason: reason,
	})

	return nil
}

// malformHTLC fails the peer's HTLC with the given ID as malformed.
func (n *legacyNode) malformHTLC(id uint64) error {
	code := lnwire.CodeInvalidOnionVersion
	var sha [32]byte
	err := n.lc.MalformedFailHTLC(id, code, sha, nil)
	if err != nil {
		return err
	}
	n.send(&lnwire.UpdateFailMalformedHTLC{
		ChanID:       n.lc.ChannelID(),
		ID:           id,
		ShaOnionBlob: sha,
		FailureCode:  code,
	})

	return nil
}

// updateFee proposes a new fee rate.
func (n *legacyNode) updateFee(rate chainfee.SatPerKWeight) error {
	if err := n.lc.UpdateFee(rate); err != nil {
		return err
	}
	n.send(&lnwire.UpdateFee{
		ChanID:   n.lc.ChannelID(),
		FeePerKw: uint32(rate),
	})

	return nil
}

// signCommitment signs a new remote commitment if we owe one and the window
// is open, as the link's commit ticker does.
func (n *legacyNode) signCommitment() error {
	if !n.lc.OweCommitment() {
		return nil
	}

	newCommit, err := n.lc.SignNextCommitment(context.Background())
	if errors.Is(err, lnwallet.ErrNoWindow) {
		return nil
	}
	if err != nil {
		return n.fail(err)
	}
	records, err := lnwire.ParseCustomRecords(newCommit.AuxSigBlob)
	if err != nil {
		return n.fail(err)
	}
	n.send(&lnwire.CommitSig{
		ChanID:        n.lc.ChannelID(),
		CommitSig:     newCommit.CommitSig,
		HtlcSigs:      newCommit.HtlcSigs,
		PartialSig:    newCommit.PartialSig,
		CustomRecords: records,
	})

	return nil
}

// receive handles a message from the peer the way the link does. An error
// fails the channel.
func (n *legacyNode) receive(msg lnwire.Message) error {
	if n.failed != nil {
		return n.failed
	}

	switch m := msg.(type) {
	case *lnwire.UpdateAddHTLC:
		if _, err := n.lc.ReceiveHTLC(m); err != nil {
			return n.fail(err)
		}

	case *lnwire.UpdateFulfillHTLC:
		err := n.lc.ReceiveHTLCSettle(m.PaymentPreimage, m.ID)
		if err != nil {
			return n.fail(err)
		}

	case *lnwire.UpdateFailHTLC:
		if err := n.lc.ReceiveFailHTLC(m.ID, m.Reason); err != nil {
			return n.fail(err)
		}

	case *lnwire.UpdateFailMalformedHTLC:
		if m.FailureCode&lnwire.FlagBadOnion == 0 {
			return n.fail(errors.New("malformed fail without " +
				"BADONION"))
		}
		reason, err := malformedReason(m)
		if err != nil {
			return n.fail(err)
		}
		if err := n.lc.ReceiveFailHTLC(m.ID, reason); err != nil {
			return n.fail(err)
		}

	case *lnwire.UpdateFee:
		if n.lc.IsInitiator() {
			return n.fail(errors.New("fee update as initiator"))
		}
		rate := chainfee.SatPerKWeight(m.FeePerKw)
		if err := n.lc.ReceiveUpdateFee(rate); err != nil {
			return n.fail(err)
		}

	case *lnwire.CommitSig:
		auxSigBlob, err := m.CustomRecords.Serialize()
		if err != nil {
			return n.fail(err)
		}
		err = n.lc.ReceiveNewCommitment(&lnwallet.CommitSigs{
			CommitSig:  m.CommitSig,
			HtlcSigs:   m.HtlcSigs,
			PartialSig: m.PartialSig,
			AuxSigBlob: auxSigBlob,
		})
		if err != nil {
			return n.fail(err)
		}

		raa, _, _, err := n.lc.RevokeCurrentCommitment()
		if err != nil {
			return n.fail(err)
		}
		n.send(raa)

		return n.signCommitment()

	case *lnwire.ChannelReestablish:
		return n.reestablish(m)

	case *lnwire.RevokeAndAck:
		fwd, _, err := n.lc.ReceiveRevocation(m)
		if err != nil {
			return n.fail(err)
		}
		n.fwds = append(n.fwds, fwd)

		return n.signCommitment()

	default:
		return n.fail(fmt.Errorf("unexpected message %T", msg))
	}

	return nil
}

// restart reloads the node's channel from disk, as lnd does when it
// reconnects to the peer.
func (n *legacyNode) restart() error {
	lc, err := lnwallet.RestartTestChannel(n.lc)
	if err != nil {
		return err
	}
	n.lc = lc

	return nil
}

// connect sends the node's channel_reestablish, as the link does first on
// every connection.
func (n *legacyNode) connect() error {
	msg, err := n.chanSync()
	if err != nil {
		return err
	}
	n.send(msg)

	return nil
}

// chanSync returns the node's channel_reestablish.
func (n *legacyNode) chanSync() (*lnwire.ChannelReestablish, error) {
	return n.lc.State().ChanSyncMsg()
}

// reestablish handles the peer's channel_reestablish the way the link does,
// sending whatever the channel says to retransmit.
func (n *legacyNode) reestablish(msg *lnwire.ChannelReestablish) error {
	if n.failed != nil {
		return n.failed
	}

	msgs, _, _, err := n.lc.ProcessChanSyncMsg(context.Background(), msg)
	if err != nil {
		return n.fail(err)
	}
	n.send(msgs...)

	return nil
}
