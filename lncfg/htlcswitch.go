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

	// minQuiescenceTimeout specifies the minimal timeout value that can be
	// used for `QuiescenceTimeout`.
	minQuiescenceTimeout = 30 * time.Second

	// DefaultQuiescenceTimeout specifies the default value to be used for
	// `QuiescenceTimeout`.
	DefaultQuiescenceTimeout = 60 * time.Second
)

//nolint:ll
type Htlcswitch struct {
	MailboxDeliveryTimeout time.Duration `long:"mailboxdeliverytimeout" description:"The timeout value when delivering HTLCs to a channel link. Setting this value too small will result in local payment failures if large number of payments are sent over a short period."`

	QuiescenceTimeout time.Duration `long:"quiescencetimeout" description:"The max duration that the channel can be quiesced. Any dependent protocols (dynamic commitments, splicing, etc.) must finish their operations under this timeout value, otherwise the node will disconnect."`
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

	// Skip the validation for integration tests so we can use a smaller
	// timeout value to check the timeout behavior.
	if !IsDevBuild() && h.QuiescenceTimeout < minQuiescenceTimeout {
		return fmt.Errorf("quiescencetimeout: %v below minimal: %v",
			h.QuiescenceTimeout, minQuiescenceTimeout)
	}

	return nil
}
