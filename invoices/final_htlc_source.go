package invoices

import (
	"errors"
	"fmt"

	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/lnwire"
)

// finalHtlcDB is the part of the channel state database that the final htlc
// source reads from.
type finalHtlcDB interface {
	// HistoricalChannelStore retains channel status after closure, when
	// summaries alone cannot distinguish restored commitment snapshots.
	chanstate.HistoricalChannelStore

	// LookupFinalHtlc returns the recorded final outcome of an htlc, or
	// chanstate.ErrHtlcUnknown.
	LookupFinalHtlc(chanID lnwire.ShortChannelID,
		htlcIndex uint64) (*chanstate.FinalHtlcInfo, error)

	// FetchAllChannels returns all channels that aren't closed yet.
	FetchAllChannels() ([]*chanstate.OpenChannel, error)

	// FetchClosedChannels returns the summaries of closed channels.
	FetchClosedChannels(pendingOnly bool) ([]*chanstate.ChannelCloseSummary,
		error)
}

// finalHtlcSource provides the invoice registry with the final outcome of the
// htlcs it released the preimage for. Outcomes are delivered live through the
// htlc notifier, and can be looked up from the channel state for outcomes the
// registry missed.
type finalHtlcSource struct {
	subscribe   func() (<-chan FinalHtlcOutcome, func(), error)
	chanStateDB finalHtlcDB
}

// NewFinalHtlcSource connects channel evidence to the registry's final-outcome
// subscription. The caller owns the providers; the subscription's cancel
// function must stop delivery before returning.
func NewFinalHtlcSource(db finalHtlcDB, subscribe func() (
	<-chan FinalHtlcOutcome, func(), error)) FinalHtlcSource {

	return &finalHtlcSource{
		chanStateDB: db,
		subscribe:   subscribe,
	}
}

// SubscribeFinalHtlcs delegates to the injected provider so the registry can
// receive final outcomes without depending on switch event types.
func (f *finalHtlcSource) SubscribeFinalHtlcs() (
	<-chan FinalHtlcOutcome, func(), error) {

	return f.subscribe()
}

// LookupFinalHtlcs returns the recorded final outcome of those of the given
// htlcs that have one. On-chain outcomes are always recorded, off-chain ones
// only if the user opted in to storing final resolutions.
//
// NOTE: Part of the FinalHtlcSource interface.
func (f *finalHtlcSource) LookupFinalHtlcs(keys []CircuitKey) (
	map[CircuitKey]FinalHtlcOutcome, error) {

	result := make(map[CircuitKey]FinalHtlcOutcome)
	for _, key := range keys {
		info, err := f.chanStateDB.LookupFinalHtlc(
			key.ChanID, key.HtlcID,
		)
		switch {
		case errors.Is(err, chanstate.ErrHtlcUnknown):

		case err != nil:
			return nil, fmt.Errorf("unable to look up final htlc "+
				"%v: %w", key, err)

		default:
			result[key] = FinalHtlcOutcome{
				Key:      key,
				Settled:  info.Settled,
				Offchain: info.Offchain,
			}
		}
	}

	return result, nil
}

// InferFinalHtlcs derives the final outcome of those of the given incoming
// htlcs whose outcome follows from the channel state. The htlcs must not have
// a recorded outcome.
//
// NOTE: This must only be used for incoming htlcs that we decided to settle,
// meaning that we released their preimage. Such an htlc can only leave the
// channel through our settle, so an htlc that is no longer on any commitment
// of an open channel was settled off chain. Similarly, the resolvers record
// the outcome of every htlc that is on the confirmed commitment of a closed
// channel, so an htlc of a fully resolved channel without a recorded outcome
// wasn't on the confirmed commitment because we settled it before.
//
// Restored or data-loss channels have no trusted commitments, even after
// closure, so missing HTLCs need a recorded outcome to prove payment.
// Abandoned channels can still contain unresolved HTLCs; their archived
// commitments must also show that the incoming HTLC is no longer present.
// The local commitment's next incoming HTLC index must also exceed the
// requested index: a snapshot that predates the HTLC cannot prove collection.
// Remote force closes also require the remote snapshot to have seen the HTLC.
//
// The caller must re-read recorded outcomes after this scan, because a
// resolver can persist failure while its channel becomes fully resolved.
//
// NOTE: Part of the FinalHtlcSource interface.
func (f *finalHtlcSource) InferFinalHtlcs(keys []CircuitKey) (
	map[CircuitKey]FinalHtlcOutcome, error) {

	result := make(map[CircuitKey]FinalHtlcOutcome)
	if len(keys) == 0 {
		return result, nil
	}

	// This includes pending and waiting close channels, whose commitments
	// are still in the database.
	channels, err := f.chanStateDB.FetchAllChannels()
	if err != nil {
		return nil, fmt.Errorf("unable to fetch channels: %w", err)
	}

	openChannels := make(map[lnwire.ShortChannelID]*chanstate.OpenChannel)
	for _, channel := range channels {
		openChannels[channel.ShortChanID()] = channel

		if channel.IsZeroConf() && channel.ZeroConfConfirmed() {
			openChannels[channel.ZeroConfRealScid()] = channel
		}
	}

	closedChannels, err := f.chanStateDB.FetchClosedChannels(false)
	if err != nil {
		return nil, fmt.Errorf("unable to fetch closed channels: %w",
			err)
	}

	resolvedChannels := make(
		map[lnwire.ShortChannelID]*chanstate.ChannelCloseSummary,
	)
	for _, summary := range closedChannels {
		if !summary.IsPending {
			resolvedChannels[summary.ShortChanID] = summary
		}
	}

	for _, key := range keys {
		if channel, ok := openChannels[key.ChanID]; ok {
			// Restored or stale snapshots cannot prove collection
			// from a missing HTLC.
			if channel.HasChanStatus(
				chanstate.ChanStatusRestored,
			) || channel.HasChanStatus(
				chanstate.ChanStatusLocalDataLoss,
			) {

				continue
			}

			// A missing HTLC only proves removal if this snapshot
			// has already seen the requested incoming index.
			if key.HtlcID >=
				channel.LocalCommitment.RemoteHtlcIndex {

				continue
			}

			if !incomingHtlcOnCommitment(channel, key.HtlcID) {
				result[key] = FinalHtlcOutcome{
					Key:      key,
					Settled:  true,
					Offchain: true,
				}
			}

			continue
		}

		if summary, ok := resolvedChannels[key.ChanID]; ok {
			// Closed summaries omit channel trust status. Read the
			// archived channel only for this recovery candidate.
			channel, err := f.chanStateDB.FetchHistoricalChannel(
				&summary.ChanPoint,
			)
			if err != nil {
				return nil, fmt.Errorf("unable to fetch "+
					"historical channel %v: %w",
					summary.ChanPoint, err)
			}
			if channel.HasChanStatus(
				chanstate.ChanStatusRestored,
			) || channel.HasChanStatus(
				chanstate.ChanStatusLocalDataLoss,
			) {

				continue
			}

			// A resolved archive cannot prove collection of an
			// incoming HTLC added after its snapshot.
			if key.HtlcID >=
				channel.LocalCommitment.RemoteHtlcIndex {

				continue
			}

			// The local view may include an add that the remote
			// commitment never saw, so it cannot prove collection.
			remoteIndex := channel.RemoteCommitment.RemoteHtlcIndex
			if summary.CloseType == chanstate.RemoteForceClose &&
				key.HtlcID >= remoteIndex {

				continue
			}

			// Abandonment closes records without resolving HTLCs.
			// Archived incoming HTLCs therefore remain unknown.
			if summary.CloseType == chanstate.Abandoned &&
				incomingHtlcOnCommitment(channel, key.HtlcID) {

				continue
			}

			result[key] = FinalHtlcOutcome{
				Key:     key,
				Settled: true,
			}
		}
	}

	return result, nil
}

// incomingHtlcOnCommitment returns true if the incoming htlc with the given
// index is on our current commitment or on the remote commitment that our peer
// hasn't revoked yet.
//
// NOTE: We don't need to check the pending remote commitment, which we signed
// but our peer didn't revoke the previous one for yet. An incoming htlc is
// added to our commitment before it is added to theirs, and removed from
// theirs before it is removed from ours, so it can only be on the pending
// remote commitment if it is on one of the other two.
func incomingHtlcOnCommitment(channel *chanstate.OpenChannel,
	htlcIndex uint64) bool {

	onCommit := func(commit *chanstate.ChannelCommitment) bool {
		for _, htlc := range commit.Htlcs {
			if htlc.Incoming && htlc.HtlcIndex == htlcIndex {
				return true
			}
		}

		return false
	}

	return onCommit(&channel.LocalCommitment) ||
		onCommit(&channel.RemoteCommitment)
}

// A compile-time check to ensure finalHtlcSource implements the
// FinalHtlcSource interface.
var _ FinalHtlcSource = (*finalHtlcSource)(nil)
