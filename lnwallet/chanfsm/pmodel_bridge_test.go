package chanfsm

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/lightningnetwork/lnd/protofsm"
	"github.com/stretchr/testify/require"
)

// This file replays executions of the P channel contract (pmodel/src/
// node.p) into the Go state machine. Each node of each recorded run becomes a
// state machine that starts in Synced with an empty ledger, and every input
// the model's node handled, a command or a peer message, is applied to it
// with protofsm.ApplyEvents. The channel operations the machine authorizes
// don't run against a real channel: the bridge completes each with the
// outcome the model's node saw, which for a commitment_signed or a
// revoke_and_ack is whether the channel would accept it, recorded in the
// trace.
//
// After every input, the bridge compares the machine's projected outbox with
// the model's: the messages sent (their kind and HTLC ID), the updates
// forwarded, the channel failing, and the answer to a command. The order
// within one input's outbox is not compared, since a forwarding package
// lists adds before removals where the model forwards in log order.

// pmodelTraceEnv names the environment variable holding a directory of
// freshly recorded traces. Without it, the bridge replays the checked-in
// ones in pmodel/traces.
const pmodelTraceEnv = "CHANFSM_PMODEL_TRACES"

// pStep is one input a model node handled, and its outbox.
type pStep struct {
	input string
	out   []string
}

// pRun is one node's side of one recorded run.
type pRun struct {
	node      string
	initiator bool
	steps     []pStep
}

// parsePTraces reads the runs in a trace file. A run starts at each node's
// begin line, and each run holds both nodes.
func parsePTraces(t *testing.T, path string) []*pRun {
	t.Helper()

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var (
		runs []*pRun
		cur  = map[string]*pRun{}
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		node, rest, _ := strings.Cut(line, " ")
		kind, body, _ := strings.Cut(rest, " ")

		switch kind {
		case "begin":
			r := &pRun{
				node:      node,
				initiator: body == "initiator=1",
			}
			runs = append(runs, r)
			cur[node] = r

		case "in":
			r := cur[node]
			require.NotNil(t, r, "input before begin: %q", line)
			r.steps = append(r.steps, pStep{input: body})

		case "out":
			r := cur[node]
			require.NotNil(t, r, "output before begin: %q", line)
			require.NotEmpty(t, r.steps, "output before input: %q",
				line)
			s := &r.steps[len(r.steps)-1]
			s.out = append(s.out, body)

		default:
			t.Fatalf("unknown trace line %q", line)
		}
	}
	require.NoError(t, sc.Err())

	return runs
}

// traceField returns the integer value of a key=value field.
func traceField(t *testing.T, body, key string) int {
	t.Helper()

	for _, f := range strings.Fields(body) {
		k, v, ok := strings.Cut(f, "=")
		if ok && k == key {
			n, err := strconv.Atoi(v)
			require.NoError(t, err)

			return n
		}
	}
	t.Fatalf("no field %q in %q", key, body)

	return 0
}

// bridgeInput converts a trace input into the Go event, and says whether the
// channel accepts the operation it leads to.
func bridgeInput(t *testing.T, input string) (Event, bool) {
	t.Helper()

	var (
		chanID lnwire.ChannelID
		id     = func() uint64 {
			return uint64(traceField(t, input, "id"))
		}
	)

	fields := strings.Fields(input)
	require.GreaterOrEqual(t, len(fields), 2, "input %q", input)

	switch fields[0] + " " + fields[1] {
	case "cmd add":
		return &AddHTLC{Htlc: &lnwire.UpdateAddHTLC{ChanID: chanID}},
			true

	case "cmd settle":
		return &SettleHTLC{Msg: &lnwire.UpdateFulfillHTLC{
			ChanID: chanID, ID: id(),
		}}, true

	case "cmd fail":
		return &FailHTLC{Msg: &lnwire.UpdateFailHTLC{
			ChanID: chanID, ID: id(),
		}}, true

	case "cmd malformed":
		return &MalformedFailHTLC{Msg: &lnwire.UpdateFailMalformedHTLC{
			ChanID:      chanID,
			ID:          id(),
			FailureCode: lnwire.CodeInvalidOnionVersion,
		}}, true

	case "cmd fee":
		return &UpdateFee{FeePerKw: 253}, true

	case "cmd sign":
		return &SignCommitment{}, true

	case "msg add":
		return &PeerAdd{Msg: &lnwire.UpdateAddHTLC{
			ChanID: chanID, ID: id(),
		}}, true

	case "msg settle":
		return &PeerFulfill{Msg: &lnwire.UpdateFulfillHTLC{
			ChanID: chanID, ID: id(),
		}}, true

	case "msg fail":
		return &PeerFail{Msg: &lnwire.UpdateFailHTLC{
			ChanID: chanID, ID: id(),
		}}, true

	case "msg malformed":
		code := lnwire.CodeInvalidOnionVersion
		if traceField(t, input, "badonion") == 0 {
			code = lnwire.CodeTemporaryChannelFailure
		}

		return &PeerFailMalformed{Msg: &lnwire.UpdateFailMalformedHTLC{
			ChanID: chanID, ID: id(), FailureCode: code,
		}}, true

	case "msg fee":
		return &PeerUpdateFee{Msg: &lnwire.UpdateFee{
			ChanID: chanID, FeePerKw: 253,
		}}, true

	case "msg sig":
		return &PeerCommitSig{Msg: &lnwire.CommitSig{ChanID: chanID}},
			traceField(t, input, "ok") == 1

	case "msg reest":
		return &PeerReestablish{Msg: &lnwire.ChannelReestablish{
			ChanID: chanID,
			NextLocalCommitHeight: uint64(
				traceField(t, input, "next"),
			),
			RemoteCommitTailHeight: uint64(
				traceField(t, input, "tail"),
			),
		}}, true

	case "msg raa":
		return &PeerRevokeAndAck{Msg: &lnwire.RevokeAndAck{
			ChanID: chanID,
		}}, traceField(t, input, "ok") == 1
	}

	t.Fatalf("unknown trace input %q", input)

	return nil, false
}

// errChannelRefused is what the bridge's channel returns for a signature or
// revocation the model's channel would refuse.
var errChannelRefused = errors.New("channel refused")

// bridgeOutcome completes an operation the way the model's channel does.
// The ledger is the one the machine authorized the operation from.
func bridgeOutcome(t *testing.T, op Op, l Ledger, accept bool) *OpDone {
	t.Helper()

	done := &OpDone{Op: op}
	switch o := op.(type) {
	case *opAdd:
		done.Value = l.Logs.Local.HtlcCounter

	case *opReceiveAdd:
		done.Value = o.msg.ID

	case *opSign:
		done.Value = &lnwallet.NewCommitState{
			CommitSigs: &lnwallet.CommitSigs{},
		}

	case *opReceiveCommit:
		if !accept {
			done.Err = errChannelRefused
		}

	case *opRevoke:
		done.Value = &revokeResult{msg: &lnwire.RevokeAndAck{}}

	case *opChanSync:
		done.Value = &lnwire.ChannelReestablish{
			NextLocalCommitHeight:  o.restored.local.Height + 1,
			RemoteCommitTailHeight: o.restored.remote.Tail.Height,
		}

	// The channel retransmits what the plan says; the bridge checks the
	// plan and the messages it names against the model's.
	case *opProcessSync:
		done.Value = syncedMessages(t, o.plan, l)

	case *opConfirmDataLoss:
		done.Err = errChannelRefused

	case *opReceiveRevocation:
		if !accept {
			done.Err = errChannelRefused
			break
		}

		// The channel's forwarding package holds what the ledger
		// says the revocation locks in. The bridge compares what the
		// machine then forwards with what the model forwards.
		_, fwds, err := l.ReceiveRevocation()
		require.NoError(t, err)
		done.Value = &revocationResult{pkg: fwdPkgOf(fwds)}
	}

	return done
}

// syncedMessages builds the messages a plan retransmits, taking the resent
// updates from the ledger's log.
func syncedMessages(t *testing.T, plan SyncPlan, l Ledger) []lnwire.Message {
	t.Helper()

	var updates []uint64
	switch p := plan.(type) {
	case ResendCommitment:
		updates = p.Updates
	case ResendBoth:
		updates = p.Updates
	}

	var msgs []lnwire.Message
	for _, kind := range plan.Messages() {
		switch kind {
		case "raa":
			msgs = append(msgs, &lnwire.RevokeAndAck{})

		case "sig":
			msgs = append(msgs, &lnwire.CommitSig{})

		default:
			idx := updates[0]
			updates = updates[1:]
			i := slices.IndexFunc(l.Logs.Local.Entries,
				func(e Entry) bool { return e.LogIndex == idx })
			require.GreaterOrEqual(t, i, 0, "no update %d", idx)
			e := l.Logs.Local.Entries[i]

			switch e.Kind {
			case KindAdd:
				msgs = append(msgs, &lnwire.UpdateAddHTLC{
					ID: e.HtlcIndex,
				})
			case KindSettle:
				msgs = append(msgs, &lnwire.UpdateFulfillHTLC{
					ID: e.ParentIndex,
				})
			case KindFail:
				msgs = append(msgs, &lnwire.UpdateFailHTLC{
					ID: e.ParentIndex,
				})
			case KindMalformed:
				msgs = append(msgs,
					&lnwire.UpdateFailMalformedHTLC{
						ID: e.ParentIndex,
					})
			case KindFee:
				msgs = append(msgs, &lnwire.UpdateFee{})
			}
		}
	}

	return msgs
}

// fwdPkgOf builds the forwarding package holding the given updates.
func fwdPkgOf(fwds []ForwardRef) *channeldb.FwdPkg {
	var adds, settleFails []channeldb.LogUpdate
	for _, f := range fwds {
		switch f.Kind {
		case KindAdd:
			adds = append(adds, channeldb.LogUpdate{
				UpdateMsg: &lnwire.UpdateAddHTLC{ID: f.ID},
			})

		case KindSettle:
			settleFails = append(settleFails, channeldb.LogUpdate{
				UpdateMsg: &lnwire.UpdateFulfillHTLC{ID: f.ID},
			})

		default:
			settleFails = append(settleFails, channeldb.LogUpdate{
				UpdateMsg: &lnwire.UpdateFailHTLC{ID: f.ID},
			})
		}
	}

	return channeldb.NewFwdPkg(
		lnwire.ShortChannelID{}, 0, adds, settleFails,
	)
}

// projectOutbox renders the outbox in the model's terms.
func projectOutbox(outbox []Outbox) []string {
	var out []string
	for _, o := range outbox {
		switch o := o.(type) {
		case *SendToPeer:
			for _, m := range o.Msgs {
				out = append(out, "send "+projectMsg(m))
			}

		case *ForwardPackage:
			for _, f := range forwardsOf(o.Pkg) {
				out = append(out, fmt.Sprintf("fwd %s id=%d",
					strings.ToLower(f.Kind.String()),
					f.ID))
			}

		case *FailChannel:
			out = append(out, "fail")

		case *Reply:
			if o.Err != nil {
				out = append(out, "reply err")
			} else {
				out = append(out, "reply ok")
			}
		}
	}

	return out
}

// projectMsg renders a message in the model's terms.
func projectMsg(m lnwire.Message) string {
	switch m := m.(type) {
	case *lnwire.UpdateAddHTLC:
		return fmt.Sprintf("add id=%d", m.ID)
	case *lnwire.UpdateFulfillHTLC:
		return fmt.Sprintf("settle id=%d", m.ID)
	case *lnwire.UpdateFailHTLC:
		return fmt.Sprintf("fail id=%d", m.ID)
	case *lnwire.UpdateFailMalformedHTLC:
		return fmt.Sprintf("malformed id=%d", m.ID)
	case *lnwire.UpdateFee:
		return "fee id=0"
	case *lnwire.CommitSig:
		return "sig"
	case *lnwire.RevokeAndAck:
		return "raa"
	case *lnwire.ChannelReestablish:
		return fmt.Sprintf("reest next=%d tail=%d",
			m.NextLocalCommitHeight, m.RemoteCommitTailHeight)
	}

	return fmt.Sprintf("%T", m)
}

// replayPRun replays one node's run into the Go state machine.
func replayPRun(t *testing.T, r *pRun) {
	t.Helper()

	initiator := lntypes.Remote
	if r.initiator {
		initiator = lntypes.Local
	}
	var (
		ctx          = t.Context()
		env          = &Env{}
		state        = stateFor(Ledger{Initiator: initiator})
		observe      = tableObserver(t)
		observeTable = protofsm.TransitionObserver[
			Event, Outbox, *Env,
		](observe)
	)

	for i, step := range r.steps {
		var (
			event  Event
			accept = true
		)

		// A restart reloads the channel from disk: the machine starts
		// over from the restored ledger, and connects.
		if step.input == "restart" {
			switch s := state.(type) {
			case *Failed:
				require.Empty(t, step.out, "%s step %d",
					r.node, i)
				continue

			case *Connecting:
				state = &Connecting{
					Restored: s.Restored.ledger().Restore(),
				}

			case *Reestablishing:
				state = &Connecting{
					Restored: s.Restored.ledger().Restore(),
				}

			default:
				l, ok := ledgerOf(state)
				require.True(t, ok, "restart in %v", state)
				state = &Connecting{Restored: l.Restore()}
			}
			event = &Connect{}
		} else {
			event, accept = bridgeInput(t, step.input)
		}

		var outbox []Outbox
		for event != nil {
			next, out, err := protofsm.ApplyEventsObserved(
				ctx, state, event, env, observeTable,
			)
			require.NoError(t, err, "%s step %d: %s", r.node, i,
				step.input)

			event = nil
			for _, o := range out {
				apply, ok := o.(*ApplyOp)
				if !ok {
					outbox = append(outbox, o)
					continue
				}
				l, _ := ledgerOf(next)
				event = bridgeOutcome(t, apply.Op, l, accept)
			}
			state = next
		}

		got, want := projectOutbox(outbox), slices.Clone(step.out)
		slices.Sort(got)
		slices.Sort(want)
		require.Equal(t, want, got, "%s step %d: %s", r.node, i,
			step.input)
	}
}

// pmodelTraceFiles returns the trace files to replay.
func pmodelTraceFiles(t *testing.T) []string {
	t.Helper()

	dir := os.Getenv(pmodelTraceEnv)
	if dir == "" {
		dir = filepath.Join("pmodel", "traces")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.trace"))
	require.NoError(t, err)
	require.NotEmpty(t, files, "no traces in %s", dir)

	return files
}

// TestPModelBridge replays every recorded run of the P channel contract
// into the Go state machine.
func TestPModelBridge(t *testing.T) {
	t.Parallel()

	for _, path := range pmodelTraceFiles(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			runs := parsePTraces(t, path)
			require.NotEmpty(t, runs)

			var steps int
			for _, r := range runs {
				replayPRun(t, r)
				steps += len(r.steps)
			}
			t.Logf("replayed %d node runs, %d steps", len(runs),
				steps)
		})
	}
}
