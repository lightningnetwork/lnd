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

	// FwdState is an enum used to describe the lifecycle of a FwdPkg.
	FwdState = cstate.FwdState

	// PkgFilter is used to compactly represent a particular subset of the
	// Adds in a forwarding package.
	PkgFilter = cstate.PkgFilter

	// FwdPkg records all adds, settles, and fails that were locked in as a
	// result of the remote peer sending us a revocation.
	FwdPkg = cstate.FwdPkg

	// SettleFailAcker is a generic interface providing the ability to
	// acknowledge settle/fail HTLCs stored in forwarding packages.
	SettleFailAcker = cstate.SettleFailAcker

	// GlobalFwdPkgReader is an interface used to retrieve the forwarding
	// packages of any active channel.
	GlobalFwdPkgReader = cstate.GlobalFwdPkgReader

	// FwdOperator defines the interfaces for managing forwarding packages
	// that are external to a particular channel.
	FwdOperator = cstate.FwdOperator

	// FwdPackager supports all operations required to modify fwd packages,
	// such as creation, updates, reading, and removal.
	FwdPackager = cstate.FwdPackager

	// SwitchPackager is a concrete implementation of the FwdOperator
	// interface.
	SwitchPackager = cstate.SwitchPackager

	// ChannelPackager is used by a channel to manage the lifecycle of its
	// forwarding packages.
	ChannelPackager = cstate.ChannelPackager
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
