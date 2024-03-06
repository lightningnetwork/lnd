package peer

import (
	"net"
	"time"

	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
)

// messageSwitch is an interface that abstracts managing the lifecycle of
// abstract links. This is intended for peer package usage only.
type messageSwitch interface {
	// BestHeight returns the best height known to the messageSwitch.
	BestHeight() uint32

	// CircuitModifier returns a reference to the messageSwitch's internal
	// CircuitModifier which abstracts the paths payments take and allows
	// modifying them.
	CircuitModifier() htlcswitch.CircuitModifier

	// RemoveLink removes an abstract link given a ChannelID.
	RemoveLink(cid lnwire.ChannelID)

	// CreateAndAddLink creates an abstract link in the messageSwitch given
	// a ChannelLinkConfig and raw LightningChannel pointer.
	CreateAndAddLink(cfg htlcswitch.ChannelLinkConfig,
		lnChan *lnwallet.LightningChannel) error

	// GetLinksByInterface retrieves abstract links (represented by the
	// ChannelUpdateHandler interface) based on the provided public key.
	GetLinksByInterface(pub [33]byte) ([]htlcswitch.ChannelUpdateHandler,
		error)
}

// MessageConn is an interface implemented by anything that delivers
// an lnwire.Message using a net.Conn interface.
type MessageConn interface {
	// RemoteAddr returns the remote address on the other end of the connection.
	RemoteAddr() net.Addr

	// LocalAddr returns the local address on our end of the connection.
	LocalAddr() net.Addr

	// Read reads bytes from the connection.
	Read([]byte) (int, error)

	// Write writes bytes to the connection.
	Write([]byte) (int, error)

	// SetDeadline sets the deadline for the connection.
	SetDeadline(time.Time) error

	// SetReadDeadline sets the read deadline.
	SetReadDeadline(time.Time) error

	// SetWriteDeadline sets the write deadline.
	SetWriteDeadline(time.Time) error

	// Close closes the connection.
	Close() error

	// Flush attempts a flush.
	Flush() (int, error)

	// WriteMessage writes the message.
	WriteMessage([]byte) error

	// ReadNextHeader reads the next header.
	ReadNextHeader() (uint32, error)

	// ReadNextBody reads the next body.
	ReadNextBody([]byte) ([]byte, error)
}
