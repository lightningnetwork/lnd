package channeldb

import (
	cstate "github.com/lightningnetwork/lnd/chanstate"
)

type (
	// AddRef is used to identify a particular Add in a FwdPkg.
	AddRef = cstate.AddRef

	// SettleFailRef is used to locate a Settle/Fail in another channel's
	// FwdPkg.
	SettleFailRef = cstate.SettleFailRef

	// FwdPkg records all adds, settles, and fails that were locked in as a
	// result of the remote peer sending us a revocation.
	FwdPkg = cstate.FwdPkg
)

const (
	// FwdStateLockedIn is the starting state for all forwarding packages.
	FwdStateLockedIn = cstate.FwdStateLockedIn

	// FwdStateProcessed marks the state in which all Adds have been
	// locally processed.
	FwdStateProcessed = cstate.FwdStateProcessed

	// FwdStateCompleted signals that all Adds have been acked, and that
	// all settles and fails have been delivered to their sources.
	FwdStateCompleted = cstate.FwdStateCompleted
)

var (
	// fwdPackagesKey is retained while the root channeldb bucket setup
	// remains in this package.
	fwdPackagesKey = cstate.FwdPackagesBucketKey()

	// NewPkgFilter initializes an empty PkgFilter supporting `count`
	// elements.
	NewPkgFilter = cstate.NewPkgFilter

	// NewFwdPkg initializes a new forwarding package in FwdStateLockedIn.
	NewFwdPkg = cstate.NewFwdPkg

	// ErrCorruptedFwdPkg signals that the on-disk structure of the
	// forwarding package has potentially been mangled.
	ErrCorruptedFwdPkg = cstate.ErrCorruptedFwdPkg

	// NewSwitchPackager instantiates a new SwitchPackager.
	NewSwitchPackager = cstate.NewSwitchPackager

	// NewChannelPackager creates a new packager for a single channel.
	NewChannelPackager = cstate.NewChannelPackager
)
