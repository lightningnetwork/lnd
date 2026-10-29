package channeldb

import "fmt"

var (
	// ErrDBReversion is returned when detecting an attempt to revert to a
	// prior database version.
	ErrDBReversion = fmt.Errorf("channel db cannot revert to prior version")

	// ErrMetaNotFound is returned when meta bucket hasn't been
	// created.
	ErrMetaNotFound = fmt.Errorf("unable to locate meta information")

	// ErrDBVersionNotFound is returned when the meta bucket exists, but
	// the DB version key hasn't been written.
	ErrDBVersionNotFound = fmt.Errorf("unable to locate db version")

	// ErrNoForwardingEvents is returned in the case that a query fails due
	// to the log not having any recorded events.
	ErrNoForwardingEvents = fmt.Errorf("no recorded forwarding events")
)
