package lnd

import (
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/chanstate"
	"github.com/lightningnetwork/lnd/reputation"
)

// reputationChannelSource adapts the channel database and channel notifier to
// the read-only reputation.ChannelSource seam. It exposes the node's active
// channels (with their holder-side capacity limits, used to size resource
// buckets) and a stream of open/close events. It is strictly read-only.
type reputationChannelSource struct {
	chanDB   chanstate.Store
	notifier *channelnotifier.ChannelNotifier
}

// newReputationChannelSource builds a reputation.ChannelSource backed by the
// channel state db and channel notifier.
func newReputationChannelSource(chanDB chanstate.Store,
	notifier *channelnotifier.ChannelNotifier) reputation.ChannelSource {

	return &reputationChannelSource{
		chanDB:   chanDB,
		notifier: notifier,
	}
}

// channelInfo extracts the reputation-relevant capacity limits from an open
// channel, reading the holder (local) side limits (matching LDK's
// get_holder_max_*).
func channelInfo(c *channeldb.OpenChannel) reputation.ChannelInfo {
	return reputation.ChannelInfo{
		SCID:             c.ShortChanID(),
		MaxAcceptedHTLCs: c.LocalChanCfg.MaxAcceptedHtlcs,
		MaxInFlightMsat:  c.LocalChanCfg.MaxPendingAmount,
	}
}

// ActiveChannels returns the node's currently open channels and their limits.
func (r *reputationChannelSource) ActiveChannels() ([]reputation.ChannelInfo,
	error) {

	channels, err := r.chanDB.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}

	infos := make([]reputation.ChannelInfo, 0, len(channels))
	for _, c := range channels {
		infos = append(infos, channelInfo(c))
	}

	return infos, nil
}

// SubscribeChannelEvents returns a stream of open/close events and a cancel
// func. A goroutine translates the channel notifier's events into the
// reputation seam's ChannelEvent type until cancelled.
func (r *reputationChannelSource) SubscribeChannelEvents() (
	<-chan reputation.ChannelEvent, func(), error) {

	client, err := r.notifier.SubscribeChannelEvents()
	if err != nil {
		return nil, nil, err
	}

	out := make(chan reputation.ChannelEvent, 16)
	quit := make(chan struct{})

	go func() {
		defer close(out)
		for {
			select {
			case <-quit:
				return

			case e, ok := <-client.Updates():
				if !ok {
					return
				}

				ev, relay := translateChannelEvent(e)
				if !relay {
					continue
				}

				select {
				case out <- ev:
				case <-quit:
					return
				}
			}
		}
	}()

	cancel := func() {
		close(quit)
		client.Cancel()
	}

	return out, cancel, nil
}

// translateChannelEvent maps a channel notifier event to the reputation seam's
// ChannelEvent, returning relay=false for events that are not open/close.
func translateChannelEvent(e interface{}) (reputation.ChannelEvent, bool) {
	switch ev := e.(type) {
	case channelnotifier.OpenChannelEvent:
		return reputation.ChannelEvent{
			Info: channelInfo(ev.Channel),
			Open: true,
		}, true

	case channelnotifier.ClosedChannelEvent:
		return reputation.ChannelEvent{
			Info: reputation.ChannelInfo{
				SCID: ev.CloseSummary.ShortChanID,
			},
			Open: false,
		}, true

	default:
		return reputation.ChannelEvent{}, false
	}
}
