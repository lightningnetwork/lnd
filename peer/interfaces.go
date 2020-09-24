package peer

import (
	"net"
	"time"

	"github.com/lightningnetwork/lnd/lnwire"
)

// LinkUpdater is an interface implemented by most messages in BOLT 2 that are
// allowed to update the channel state.
type LinkUpdater interface {
	// TargetChanID returns the channel id of the link for which this message
	// is intended.
	TargetChanID() lnwire.ChannelID
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
