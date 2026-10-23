package chanfsm

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"
)

// leanBinEnv names the environment variable holding the path of the Lean
// model's executable, built by lean/check.sh. The differential test skips
// when it is unset, so a plain go test needs no Lean toolchain.
const leanBinEnv = "CHANFSM_LEAN_BIN"

// leanModel is a running instance of the Lean model's executable, which
// answers one case at a time over its standard input and output.
type leanModel struct {
	in  io.WriteCloser
	out *bufio.Scanner
}

// startLean starts the Lean executable, or skips the test if it isn't
// configured. The process lives until the test ends.
func startLean(t *testing.T) *leanModel {
	t.Helper()

	bin := os.Getenv(leanBinEnv)
	if bin == "" {
		t.Skipf("%s not set; run lean/check.sh", leanBinEnv)
	}

	cmd := exec.Command(bin)
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())

	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Wait()
	})

	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)

	return &leanModel{in: in, out: scanner}
}

// ask sends one case to the model and returns its answer, without the
// closing end line.
func (m *leanModel) ask(t require.TestingT, input string) []string {
	_, err := io.WriteString(m.in, input)
	require.NoError(t, err)

	var lines []string
	for m.out.Scan() {
		line := m.out.Text()
		if line == "end" {
			return lines
		}
		lines = append(lines, line)
	}
	require.NoError(t, m.out.Err())
	require.Fail(t, "lean model exited mid-case")

	return nil
}

// leanOp is one ledger operation, in the model's input format and as a Go
// call.
type leanOp struct {
	line  string
	apply func(Ledger) (Ledger, []ForwardRef, error)
}

// kindNames are the kinds in the model's numbering.
var kindNames = []Kind{KindAdd, KindSettle, KindFail, KindMalformed, KindFee}

// drawLeanOp draws a random operation. IDs are drawn near the counters so
// that most removals and adds name something real.
func drawLeanOp(t *rapid.T, l Ledger) leanOp {
	id := func(label string, counter uint64) uint64 {
		return uint64(rapid.IntRange(0, int(counter)+1).Draw(t, label))
	}
	kind := func() (Kind, int) {
		k := rapid.IntRange(0, 4).Draw(t, "kind")
		return kindNames[k], k
	}
	noFwd := func(f func(Ledger) (Ledger, error)) func(Ledger) (Ledger,
		[]ForwardRef, error) {

		return func(l Ledger) (Ledger, []ForwardRef, error) {
			next, err := f(l)
			return next, nil, err
		}
	}

	// Finishing a commitment exchange that's under way is drawn more
	// often, so that runs get far enough to lock updates in and forward
	// them.
	op := rapid.IntRange(0, 13).Draw(t, "op")
	finish := rapid.IntRange(0, 2).Draw(t, "finish") == 0
	switch {
	case finish && l.Chains.Local.Pending.IsSome():
		op = 10
	case finish && l.Chains.Remote.Pending.IsSome():
		op = 11
	}

	switch op {
	case 0, 1:
		return leanOp{"addours", noFwd(func(l Ledger) (Ledger, error) {
			id := l.Logs.Local.HtlcCounter
			return l.AddHtlc(lntypes.Local, id)
		})}

	case 2, 3:
		n := id("id", l.Logs.Remote.HtlcCounter)
		return leanOp{fmt.Sprintf("addtheirs %d", n),
			noFwd(func(l Ledger) (Ledger, error) {
				return l.AddHtlc(lntypes.Remote, n)
			})}

	case 4:
		k, kn := kind()
		n := id("id", l.Logs.Remote.HtlcCounter)
		return leanOp{fmt.Sprintf("removeours %d %d", kn, n),
			noFwd(func(l Ledger) (Ledger, error) {
				return l.RemoveHtlc(lntypes.Local, k, n)
			})}

	case 5:
		k, kn := kind()
		n := id("id", l.Logs.Local.HtlcCounter)
		return leanOp{fmt.Sprintf("removetheirs %d %d", kn, n),
			noFwd(func(l Ledger) (Ledger, error) {
				return l.RemoveHtlc(lntypes.Remote, k, n)
			})}

	case 6:
		return leanOp{"feeours", noFwd(func(l Ledger) (Ledger, error) {
			return l.UpdateFee(lntypes.Local)
		})}

	case 7:
		return leanOp{"feetheirs", noFwd(func(l Ledger) (Ledger,
			error) {

			return l.UpdateFee(lntypes.Remote)
		})}

	case 8:
		return leanOp{"sign", noFwd(Ledger.SignCommitment)}

	case 9:
		return leanOp{"recvcommit", noFwd(Ledger.ReceiveCommitment)}

	case 10:
		return leanOp{"revoke", noFwd(Ledger.RevokeCommitment)}

	case 13:
		return leanOp{"restart", noFwd(func(l Ledger) (Ledger, error) {
			return l.Restore().ledger(), nil
		})}

	default:
		return leanOp{"recvrev", Ledger.ReceiveRevocation}
	}
}

// leanErrName names an error the way the model does.
func leanErrName(err error) string {
	var (
		badID   *ErrHtlcID
		unknown *ErrUnknownHtlc
		removed *ErrHtlcRemoved
		early   *ErrHtlcNotCommitted
	)
	switch {
	case errors.As(err, &badID):
		return "badId"
	case errors.As(err, &unknown):
		return "unknown"
	case errors.As(err, &removed):
		return "removed"
	case errors.As(err, &early):
		return "notCommitted"
	case errors.Is(err, ErrFeeUpdateNotInitiator):
		return "notInitiator"
	case errors.Is(err, ErrNoRevocationWindow):
		return "noWindow"
	case errors.Is(err, ErrUnexpectedCommitment):
		return "unexpectedCommit"
	case errors.Is(err, ErrNothingToRevoke):
		return "nothingToRevoke"
	case errors.Is(err, ErrUnexpectedRevocation):
		return "unexpectedRevocation"
	case strings.Contains(err.Error(), "is not a removal"):
		return "notRemoval"
	}

	return "unmapped: " + err.Error()
}

// kindNum numbers a kind the way the model does.
func kindNum(k Kind) int {
	for i, kk := range kindNames {
		if kk == k {
			return i
		}
	}

	return -1
}

// dumpLedger renders a ledger the way the model's dump does.
func dumpLedger(l Ledger) []string {
	log := func(name string, g Log) string {
		es := make([]string, 0, len(g.Entries))
		for _, e := range g.Entries {
			htlc := e.HtlcIndex
			if e.Kind.isRemoval() {
				htlc = e.ParentIndex
			}
			es = append(es, fmt.Sprintf("%d %d %d %d %d",
				e.LogIndex, kindNum(e.Kind), htlc,
				e.Heights.Local, e.Heights.Remote))
		}
		mod := make([]string, 0, len(g.Modified))
		for _, m := range g.Modified {
			mod = append(mod, fmt.Sprint(m))
		}

		return fmt.Sprintf("%s %d %d %d %s mod %s", name, g.LogIndex,
			g.HtlcCounter, len(g.Entries), strings.Join(es, " "),
			strings.Join(mod, " "))
	}
	commit := func(c Commit) string {
		return fmt.Sprintf("%d %d %d", c.Height, c.MsgIdx.Local,
			c.MsgIdx.Remote)
	}
	chain := func(name string, c Chain) string {
		if c.Pending.IsNone() {
			return fmt.Sprintf("%s %s none", name, commit(c.Tail))
		}
		p := c.Pending.UnwrapOr(Commit{})

		return fmt.Sprintf("%s %s pending %s", name, commit(c.Tail),
			commit(p))
	}

	return []string{
		log("ours", l.Logs.Local), log("theirs", l.Logs.Remote),
		chain("lc", l.Chains.Local), chain("rc", l.Chains.Remote),
	}
}

// TestLeanDiffLedger runs random operation sequences on the Go ledger and on
// the Lean model, and requires the same outcome for every operation and the
// same ledger at the end.
func TestLeanDiffLedger(t *testing.T) {
	lean := startLean(t)

	var forwards, refusals, restarts int
	rapid.Check(t, func(rt *rapid.T) {
		initiator := rapid.Bool().Draw(rt, "initiator")
		l := Ledger{Initiator: lntypes.Remote}
		init := 0
		if initiator {
			l.Initiator = lntypes.Local
			init = 1
		}

		var (
			input strings.Builder
			want  []string
		)
		fmt.Fprintf(&input, "init %d\n", init)

		n := rapid.IntRange(1, 80).Draw(rt, "ops")
		for i := 0; i < n; i++ {
			op := drawLeanOp(rt, l)
			input.WriteString(op.line + "\n")

			next, fwds, err := op.apply(l)
			if err != nil {
				want = append(want, "err "+leanErrName(err))
				refusals++
				continue
			}
			line := "ok"
			for _, f := range fwds {
				line += fmt.Sprintf(" %d %d", kindNum(f.Kind),
					f.ID)
				forwards++
			}
			want = append(want, line)
			if op.line == "restart" {
				want = append(want, dumpLedger(next)...)
				if len(l.Logs.Local.Entries) > 0 &&
					len(l.Logs.Remote.Entries) > 0 {

					restarts++
				}
			}
			l = next
		}
		input.WriteString("end\n")
		want = append(want, dumpLedger(l)...)

		got := lean.ask(rt, input.String())
		require.Equal(rt, want, got, "input:\n%s", input.String())
	})

	t.Logf("rapid: %d forwards, %d refusals, %d restarts with both "+
		"logs in use", forwards, refusals, restarts)
}
