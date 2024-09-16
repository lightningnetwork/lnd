package lncfg

import (
	"fmt"
	"time"
)

var (
	// MaxMailboxDeliveryTimeout specifies the max allowed timeout value.
	// This value is derived from the itest `async_bidirectional_payments`,
	// where both side send 483 payments at the same time to stress test
	// lnd.
	MaxMailboxDeliveryTimeout = 2 * time.Minute
)

//nolint:ll
type Htlcswitch struct {
	MailboxDeliveryTimeout time.Duration `long:"mailboxdeliverytimeout" description:"The timeout value when delivering HTLCs to a channel link. Setting this value too small will result in local payment failures if large number of payments are sent over a short period."`

	// RemoteTracking determines whether HTLC attempts should be considered
	// as tracked remotely. If true, no automated cleanup of the attempt
	// results will occur and the switch will treat all attempts as if they
	// are managed by a remote controller.
	RemoteTracking bool `long:"remotetracking" description:"If true, all HTLC attempts will be marked as remotely tracked, allowing for external control over when the attempts are marked for deletion."`
}

// Validate checks the values configured for htlcswitch.
func (h *Htlcswitch) Validate() error {
	if h.MailboxDeliveryTimeout <= 0 {
		return fmt.Errorf("mailboxdeliverytimeout must be positive")
	}

	if h.MailboxDeliveryTimeout > MaxMailboxDeliveryTimeout {
		return fmt.Errorf("mailboxdeliverytimeout: %v exceeds "+
			"maximum: %v", h.MailboxDeliveryTimeout,
			MaxMailboxDeliveryTimeout)
	}

	return nil
}
