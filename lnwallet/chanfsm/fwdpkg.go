package chanfsm

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwire"
)

// forwardsOf lists the updates of a forwarding package in the ledger's
// terms: the adds, then the settles and fails, each in log order. A malformed
// fail from the peer is recorded as a fail, so a package never holds one.
func forwardsOf(pkg *channeldb.FwdPkg) []ForwardRef {
	var fwds []ForwardRef
	for _, u := range pkg.Adds {
		if add, ok := u.UpdateMsg.(*lnwire.UpdateAddHTLC); ok {
			fwds = append(fwds, ForwardRef{
				Kind: KindAdd, ID: add.ID,
			})
		}
	}
	for _, u := range pkg.SettleFails {
		switch m := u.UpdateMsg.(type) {
		case *lnwire.UpdateFulfillHTLC:
			fwds = append(fwds, ForwardRef{
				Kind: KindSettle, ID: m.ID,
			})
		case *lnwire.UpdateFailHTLC:
			fwds = append(fwds, ForwardRef{
				Kind: KindFail, ID: m.ID,
			})
		case *lnwire.UpdateFailMalformedHTLC:
			fwds = append(fwds, ForwardRef{
				Kind: KindMalformed, ID: m.ID,
			})
		}
	}

	return fwds
}

// sameForwards reports whether two lists hold the same forwards, in any
// order. lnd rebuilds a restored log in commitment order rather than log
// order, so a forwarding package after a restart may list its updates in a
// different order than the ledger does.
func sameForwards(a, b []ForwardRef) bool {
	key := func(f ForwardRef) [2]uint64 {
		return [2]uint64{uint64(f.Kind), f.ID}
	}
	sorted := func(fs []ForwardRef) []ForwardRef {
		fs = slices.Clone(fs)
		slices.SortFunc(fs, func(x, y ForwardRef) int {
			kx, ky := key(x), key(y)
			if c := cmp.Compare(kx[0], ky[0]); c != 0 {
				return c
			}

			return cmp.Compare(kx[1], ky[1])
		})

		return fs
	}

	return slices.Equal(sorted(a), sorted(b))
}

// syncMessages describes messages retransmitted during channel_reestablish:
// "raa", "sig", and each update by its kind and the HTLC it adds or removes.
func syncMessages(msgs []lnwire.Message) []string {
	var out []string
	for _, m := range msgs {
		switch m := m.(type) {
		case *lnwire.RevokeAndAck:
			out = append(out, "raa")
		case *lnwire.CommitSig:
			out = append(out, "sig")
		case *lnwire.UpdateAddHTLC:
			out = append(out, fmt.Sprintf("add %d", m.ID))
		case *lnwire.UpdateFulfillHTLC:
			out = append(out, fmt.Sprintf("settle %d", m.ID))
		case *lnwire.UpdateFailHTLC:
			out = append(out, fmt.Sprintf("fail %d", m.ID))
		case *lnwire.UpdateFailMalformedHTLC:
			out = append(out, fmt.Sprintf("malformed %d", m.ID))
		case *lnwire.UpdateFee:
			out = append(out, "fee")
		default:
			out = append(out, fmt.Sprintf("%T", m))
		}
	}

	return out
}

// planMessages renders what a plan sends the way syncMessages renders
// messages, naming each resent update by the HTLC it adds or removes, which
// it looks up in the ledger's log.
func planMessages(l Ledger, p SyncPlan) []string {
	var updates []uint64
	switch p := p.(type) {
	case ResendCommitment:
		updates = p.Updates
	case ResendBoth:
		updates = p.Updates
	}

	var out []string
	for _, m := range p.Messages() {
		if m != "update" {
			out = append(out, m)
			continue
		}

		idx := updates[0]
		updates = updates[1:]
		i := slices.IndexFunc(l.Logs.Local.Entries, func(e Entry) bool {
			return e.LogIndex == idx
		})
		if i < 0 {
			out = append(out, fmt.Sprintf("missing %d", idx))
			continue
		}

		e := l.Logs.Local.Entries[i]
		switch e.Kind {
		case KindAdd:
			out = append(out, fmt.Sprintf("add %d", e.HtlcIndex))
		case KindSettle:
			out = append(out, fmt.Sprintf("settle %d",
				e.ParentIndex))
		case KindFail:
			out = append(out, fmt.Sprintf("fail %d",
				e.ParentIndex))
		case KindMalformed:
			out = append(out, fmt.Sprintf("malformed %d",
				e.ParentIndex))
		default:
			out = append(out, "fee")
		}
	}

	return out
}

// sortForwards orders forwards the way a forwarding package does: adds
// first, then removals, each in log order.
func sortForwards(fwds []ForwardRef) []ForwardRef {
	var adds, rmvs []ForwardRef
	for _, f := range fwds {
		if f.Kind == KindAdd {
			adds = append(adds, f)
		} else {
			rmvs = append(rmvs, f)
		}
	}

	return append(adds, rmvs...)
}
