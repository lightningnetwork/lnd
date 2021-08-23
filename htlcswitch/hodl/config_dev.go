//go:build dev
// +build dev

package hodl

// Config is a struct enumerating the possible command line flags that are used
// to activate specific hodl modes.
//
// NOTE: THESE FLAGS ARE INTENDED FOR TESTING PURPOSES ONLY. ACTIVATING THESE
// FLAGS IN PRODUCTION WILL VIOLATE CRITICAL ASSUMPTIONS MADE BY THIS SOFTWARE.
type Config struct {
	ExitSettle bool `long:"exit-settle" description:"Instructs the node to drop ADDs for which it is the exit node, and to not settle back to the sender"`

	AddIncoming bool `long:"add-incoming" description:"Instructs the node to drop incoming ADDs before processing them in the incoming link"`

	SettleIncoming bool `long:"settle-incoming" description:"Instructs the node to drop incoming SETTLEs before processing them in the incoming link"`

	FailIncoming bool `long:"fail-incoming" description:"Instructs the node to drop incoming FAILs before processing them in the incoming link"`

	AddOutgoing bool `long:"add-outgoing" description:"Instructs the node to drop outgoing ADDs before applying them to the channel state"`

	SettleOutgoing bool `long:"settle-outgoing" description:"Instructs the node to drop outgoing SETTLEs before applying them to the channel state"`

	FailOutgoing bool `long:"fail-outgoing" description:"Instructs the node to drop outgoing FAILs before applying them to the channel state"`

	Commit bool `long:"commit" description:"Instructs the node to add HTLCs to its local commitment state and to open circuits for any ADDs, but abort before committing the changes"`

	BogusSettle bool `long:"bogus-settle" description:"Instructs the node to settle back any incoming HTLC with a bogus preimage"`
}

// Mask extracts the flags specified in the configuration, composing a Mask from
// the active flags.
func (c *Config) Mask() Mask {
	var flags []Flag

	if c.ExitSettle {
		flags = append(flags, ExitSettle)
	}
	if c.AddIncoming {
		flags = append(flags, AddIncoming)
	}
	if c.SettleIncoming {
		flags = append(flags, SettleIncoming)
	}
	if c.FailIncoming {
		flags = append(flags, FailIncoming)
	}
	if c.AddOutgoing {
		flags = append(flags, AddOutgoing)
	}
	if c.SettleOutgoing {
		flags = append(flags, SettleOutgoing)
	}
	if c.FailOutgoing {
		flags = append(flags, FailOutgoing)
	}
	if c.Commit {
		flags = append(flags, Commit)
	}
	if c.BogusSettle {
		flags = append(flags, BogusSettle)
	}

	// NOTE: The value returned here will only honor the configuration if
	// the dev build flag is present. In production, this method always
	// returns hodl.MaskNone and Active(*) always returns false.
	return MaskFromFlags(flags...)
}
