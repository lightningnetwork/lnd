package lnd

import (
	"errors"

	"github.com/lightningnetwork/lnd/fn/v2"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwire"
)

var (
	// ErrCannotResume is returned when an intercepted forward cannot be
	// resumed. This is the case in the on-chain resolution flow.
	ErrCannotResume = errors.New("cannot resume in the on-chain flow")

	// ErrCannotFail is returned when an intercepted forward cannot be failed.
	// This is the case in the on-chain resolution flow.
	ErrCannotFail = errors.New("cannot fail in the on-chain flow")

	// ErrPreimageMismatch is returned when the preimage that is specified to
	// settle an htlc doesn't match the htlc hash.
	ErrPreimageMismatch = errors.New("preimage does not match hash")
)

// interceptedForward implements the on-chain behavior for the resolution of
// a forwarded htlc.
type interceptedForward struct {
	packet *htlcswitch.InterceptedPacket
	beacon *preimageBeacon
}

func newInterceptedForward(
	packet *htlcswitch.InterceptedPacket,
	beacon *preimageBeacon) *interceptedForward {

	return &interceptedForward{
		beacon: beacon,
		packet: packet,
	}
}

// Packet returns the intercepted htlc packet.
func (f *interceptedForward) Packet() htlcswitch.InterceptedPacket {
	return *f.packet
}

// Resume notifies the intention to resume an existing hold forward. This
// basically means the caller wants to resume with the default behavior for this
// htlc which usually means forward it.
func (f *interceptedForward) Resume() error {
	return ErrCannotResume
}

// ResumeModified notifies the intention to resume an existing hold forward with
// a modified htlc.
func (f *interceptedForward) ResumeModified(_, _ fn.Option[lnwire.MilliSatoshi],
	_ fn.Option[lnwire.CustomRecords]) error {

	return ErrCannotResume
}

// Fail notifies the intention to fail an existing hold forward with an
// encrypted failure reason.
func (f *interceptedForward) Fail(_ []byte) error {
	// We can't actively fail an htlc. The best we could do is abandon the
	// resolver, but this wouldn't be a safe operation. There may be a race
	// with the preimage beacon supplying a preimage. Therefore we don't
	// attempt to fail and just return an error here.
	return ErrCannotFail
}

// FailWithCode notifies the intention to fail an existing hold forward with the
// specified failure code.
func (f *interceptedForward) FailWithCode(_ lnwire.FailCode) error {
	return ErrCannotFail
}

// Settle notifies the intention to settle an existing hold forward with a given
// preimage.
func (f *interceptedForward) Settle(preimage lntypes.Preimage) error {
	if !preimage.Matches(f.packet.Hash) {
		return ErrPreimageMismatch
	}

	// Add preimage to the preimage beacon. The onchain resolver will pick
	// up the preimage from the beacon.
	return f.beacon.AddPreimages(preimage)
}
