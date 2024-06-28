package lntypes

import "fmt"

// ChannelParty is a type used to have an unambiguous description of which node
// is being referred to. This eliminates the need to describe as "local" or
// "remote" using bool.
type ChannelParty byte

const (
	// Local is a ChannelParty constructor that is used to refer to the
	// node that is running.
	Local ChannelParty = iota

	// Remote is a ChannelParty constructor that is used to refer to the
	// node on the other end of the peer connection.
	Remote
)

// Not inverts the role of the ChannelParty.
func (p ChannelParty) CounterParty() ChannelParty {
	return ChannelParty((byte(p) + 1) % 2)
}

// Dual represents a structure when we are tracking the same parameter for both
// the Local and Remote parties.
type Dual[A any] struct {
	// Local is the value tracked for the Local ChannelParty
	Local A

	// Remote is the value tracked for the Remote ChannelParty
	Remote A
}

// ForParty gives Dual an access method that takes a ChannelParty as an
// argument. It is included for ergonomics in cases where the ChannelParty is
// in a variable and which party determines how we want to access the Dual.
func (d Dual[A]) ForParty(p ChannelParty) A {
	switch p {
	case Local:
		return d.Local
	case Remote:
		return d.Remote
	default:
		panic(
			fmt.Sprintf(
				"switch default triggered in ForParty: %v", p,
			),
		)
	}
}

// MapDual applies the function argument to both the Local and Remote fields of
// the Dual[A] and returns a Dual[B] with that function applied.
func MapDual[A, B any](f func(A) B, d Dual[A]) Dual[B] {
	return Dual[B]{
		Local:  f(d.Local),
		Remote: f(d.Remote),
	}
}
