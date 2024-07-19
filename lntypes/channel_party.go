package lntypes

import "fmt"

// ChannelParty is a type used to have an unambiguous description of which node
// is being referred to. This eliminates the need to describe as "local" or
// "remote" using bool.
type ChannelParty uint8

const (
	// Local is a ChannelParty constructor that is used to refer to the
	// node that is running.
	Local ChannelParty = iota

	// Remote is a ChannelParty constructor that is used to refer to the
	// node on the other end of the peer connection.
	Remote
)

// String provides a string representation of ChannelParty (useful for logging).
func (p ChannelParty) String() string {
	switch p {
	case Local:
		return "Local"
	case Remote:
		return "Remote"
	default:
		panic(fmt.Sprintf("invalid ChannelParty value: %d", p))
	}
}

// CounterParty inverts the role of the ChannelParty.
func (p ChannelParty) CounterParty() ChannelParty {
	switch p {
	case Local:
		return Remote
	case Remote:
		return Local
	default:
		panic(fmt.Sprintf("invalid ChannelParty value: %v", p))
	}
}

// IsLocal returns true if the ChannelParty is Local.
func (p ChannelParty) IsLocal() bool {
	return p == Local
}

// IsRemote returns true if the ChannelParty is Remote.
func (p ChannelParty) IsRemote() bool {
	return p == Remote
}

// Dual represents a structure when we are tracking the same parameter for both
// the Local and Remote parties.
type Dual[A any] struct {
	// Local is the value tracked for the Local ChannelParty.
	Local A

	// Remote is the value tracked for the Remote ChannelParty.
	Remote A
}

// GetForParty gives Dual an access method that takes a ChannelParty as an
// argument. It is included for ergonomics in cases where the ChannelParty is
// in a variable and which party determines how we want to access the Dual.
func (d *Dual[A]) GetForParty(p ChannelParty) A {
	switch p {
	case Local:
		return d.Local
	case Remote:
		return d.Remote
	default:
		panic(fmt.Sprintf(
			"switch default triggered in ForParty: %v", p,
		))
	}
}

// SetForParty sets the value in the Dual for the given ChannelParty. This
// returns a copy of the original value.
func (d *Dual[A]) SetForParty(p ChannelParty, value A) {
	switch p {
	case Local:
		d.Local = value
	case Remote:
		d.Remote = value
	default:
		panic(fmt.Sprintf(
			"switch default triggered in ForParty: %v", p,
		))
	}
}

// ModifyForParty applies the function argument to the given ChannelParty field
// and returns a new copy of the Dual.
func (d *Dual[A]) ModifyForParty(p ChannelParty, f func(A) A) A {
	switch p {
	case Local:
		d.Local = f(d.Local)
		return d.Local
	case Remote:
		d.Remote = f(d.Remote)
		return d.Remote
	default:
		panic(fmt.Sprintf(
			"switch default triggered in ForParty: %v", p,
		))
	}
}

// MapDual applies the function argument to both the Local and Remote fields of
// the Dual[A] and returns a Dual[B] with that function applied.
func MapDual[A, B any](d Dual[A], f func(A) B) Dual[B] {
	return Dual[B]{
		Local:  f(d.Local),
		Remote: f(d.Remote),
	}
}

var BothParties []ChannelParty = []ChannelParty{Local, Remote}
