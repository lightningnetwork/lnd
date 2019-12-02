package htlcnotifier

type Notifier interface {
	// NotifyForwardingEvent notifies the HTLCNotifier than a HTLC has been
	// forwarded.
	NotifyForwardingEvent(event ForwardingEvent)

	// NotifyLinkFailEvent notifies the HTLCNotifier that we have failed a HTLC on
	// one of our links.
	NotifyLinkFailEvent(event LinkFailEvent)

	// NotifyForwardingFailEvent notifies the HTLCNotifier that a HTLC we forwarded
	// has failed down the line.
	NotifyForwardingFailEvent(event ForwardingFailEvent)

	// NotifySettleEvent notifies the HTLCNotifier that a HTLC that we committed to
	// as part of a forward or a receive to our node has been settled.
	NotifySettleEvent(event SettleEvent)
}
