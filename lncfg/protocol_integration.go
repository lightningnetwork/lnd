//go:build integration

package lncfg

// ProtocolOptions is a struct that we use to be able to test backwards
// compatibility of protocol additions, while defaulting to the latest within
// lnd, or to enable experimental protocol changes.
//
// TODO(yy): delete this build flag to unify with `lncfg/protocol.go`.
//
//nolint:lll
type ProtocolOptions struct {
	// LegacyProtocol is a sub-config that houses all the legacy protocol
	// options.  These are mostly used for integration tests as most modern
	// nodes should always run with them on by default.
	LegacyProtocol `group:"legacy" namespace:"legacy"`

	// ExperimentalProtocol is a sub-config that houses any experimental
	// protocol features that also require a build-tag to activate.
	ExperimentalProtocol

	// WumboChans should be set if we want to enable support for wumbo
	// (channels larger than 0.16 BTC) channels, which is the opposite of
	// mini.
	WumboChans bool `long:"wumbo-channels" description:"if set, then lnd will create and accept requests for channels larger chan 0.16 BTC"`

	// Anchors enables anchor commitments.
	// TODO(halseth): transition itests to anchors instead!
	Anchors bool `long:"anchors" description:"enable support for anchor commitments"`

	// ScriptEnforcedLease enables script enforced commitments for channel
	// leases.
	//
	// TODO: Move to experimental?
	ScriptEnforcedLease bool `long:"script-enforced-lease" description:"enable support for script enforced lease commitments"`

	// OptionScidAlias should be set if we want to signal the
	// option-scid-alias feature bit. This allows scid aliases and the
	// option-scid-alias channel-type.
	OptionScidAlias bool `long:"option-scid-alias" description:"enable support for option_scid_alias channels"`

	// OptionZeroConf should be set if we want to signal the zero-conf
	// feature bit.
	OptionZeroConf bool `long:"zero-conf" description:"enable support for zero-conf channels, must have option-scid-alias set also"`

	// NoOptionAnySegwit should be set to true if we don't want to use any
	// Taproot (and beyond) addresses for co-op closing.
	NoOptionAnySegwit bool `long:"no-any-segwit" description:"disallow using any segiwt witness version as a co-op close address"`
}

// Wumbo returns true if lnd should permit the creation and acceptance of wumbo
// channels.
func (l *ProtocolOptions) Wumbo() bool {
	return l.WumboChans
}

// NoAnchorCommitments returns true if we have disabled support for the anchor
// commitment type.
func (l *ProtocolOptions) NoAnchorCommitments() bool {
	return !l.Anchors
}

// NoScriptEnforcementLease returns true if we have disabled support for the
// script enforcement commitment type for leased channels.
func (l *ProtocolOptions) NoScriptEnforcementLease() bool {
	return !l.ScriptEnforcedLease
}

// ScidAlias returns true if we have enabled the option-scid-alias feature bit.
func (l *ProtocolOptions) ScidAlias() bool {
	return l.OptionScidAlias
}

// ZeroConf returns true if we have enabled the zero-conf feature bit.
func (l *ProtocolOptions) ZeroConf() bool {
	return l.OptionZeroConf
}

// NoAnySegwit returns true if we don't signal that we understand other newer
// segwit witness versions for co-op close addresses.
func (l *ProtocolOptions) NoAnySegwit() bool {
	return l.NoOptionAnySegwit
}
