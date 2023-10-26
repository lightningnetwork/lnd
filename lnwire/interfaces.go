package lnwire

// AnnounceSignatures is an interface that represents a message used to
// exchange signatures of a ChannelAnnouncment message during the funding flow.
type AnnounceSignatures interface {
	// SCID returns the ShortChannelID of the channel.
	SCID() ShortChannelID

	// ChanID returns the ChannelID identifying the channel.
	ChanID() ChannelID

	Message
}
