package fn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLogRecoveredPanic asserts that recovered panics use one structured log
// shape, preserve caller attributes, and handle failures during logging.
func TestLogRecoveredPanic(t *testing.T) {
	t.Parallel()

	t.Run("structured record", func(t *testing.T) {
		t.Parallel()

		logger := &capturingPanicLogger{}
		panicValue := panicTestValue{code: 7}
		LogRecoveredPanic(
			context.Background(), logger, Panic{
				Value: panicValue,
				Stack: []byte("bounded stack"),
			}, slog.String("message_type", "CommitSig"),
		)

		require.Equal(
			t, "Recovered panic", logger.msg,
		)
		require.ErrorIs(t, logger.err, errRecoveredPanic)

		attrs := make(map[string]slog.Value, len(logger.attrs))
		for _, value := range logger.attrs {
			attr, ok := value.(slog.Attr)
			require.True(t, ok)

			attrs[attr.Key] = attr.Value
		}

		require.Len(t, attrs, 4)
		require.Equal(t, "CommitSig", attrs["message_type"].String())
		require.Equal(
			t, "fn.panicTestValue", attrs["panic_type"].String(),
		)
		require.Equal(t, slog.KindAny, attrs["panic_value"].Kind())
		require.Equal(t, panicValue, attrs["panic_value"].Any())
		require.Equal(t, "bounded stack", attrs["stack"].String())
	})

	t.Run("logging panic is handled", func(t *testing.T) {
		t.Parallel()

		require.NotPanics(t, func() {
			LogRecoveredPanic(
				context.Background(), panickingPanicLogger{},
				Panic{
					Value: errors.New("boom"),
				},
			)
		})
	})
}

// TestTruncatePanicStack asserts that panic stack traces are capped with a
// readable truncation marker.
func TestTruncatePanicStack(t *testing.T) {
	t.Parallel()

	shortStack := []byte("short stack")
	require.Equal(t, shortStack, TruncatePanicStack(shortStack))

	longStack := bytes.Repeat([]byte("stack frame\n"), maxPanicStackSize)
	truncatedStack := TruncatePanicStack(longStack)

	require.LessOrEqual(t, len(truncatedStack), maxPanicStackSize)
	require.True(
		t, bytes.HasSuffix(
			truncatedStack, []byte(panicStackTruncatedMsg),
		),
	)
}

// TestRecoverPanic asserts that RecoverPanic stops a panic and reports it, that
// it stays out of the way when nothing panicked, and that the stack it hands
// back still names the function the panic actually came from.
func TestRecoverPanic(t *testing.T) {
	t.Parallel()

	t.Run("recovers and reports", func(t *testing.T) {
		t.Parallel()

		var got Panic

		func() {
			defer RecoverPanic(func(p Panic) {
				got = p
			})

			deepRecurse(0)
		}()

		require.Equal(t, "deep", got.Value)

		// The trace has to name where the panic came from, not just the
		// frame we caught it in, or it's of little use in a log.
		require.Contains(
			t, string(got.Stack), "deepRecurse",
		)
	})

	t.Run("no panic means no callback", func(t *testing.T) {
		t.Parallel()

		called := false

		func() {
			defer RecoverPanic(func(p Panic) {
				called = true
			})
		}()

		require.False(t, called)
	})

	t.Run("named return can be set", func(t *testing.T) {
		t.Parallel()

		// The common use is turning a panic into an error, which relies
		// on the callback being able to write the enclosing function's
		// named return before it actually returns.
		got := func() (err error) {
			defer RecoverPanic(func(p Panic) {
				err = fmt.Errorf("recovered panic: %v", p.Value)
			})

			panic("boom")
		}()

		require.ErrorContains(t, got, "boom")
	})

	t.Run("stack is bounded", func(t *testing.T) {
		t.Parallel()

		var got Panic

		func() {
			defer RecoverPanic(func(p Panic) {
				got = p
			})

			deepRecurse(512)
		}()

		require.LessOrEqual(t, len(got.Stack), maxPanicStackSize)
	})
}

// deepRecurse panics from the bottom of a deep call stack, so that the trace is
// long enough to need truncating.
func deepRecurse(depth int) {
	if depth == 0 {
		panic("deep")
	}

	deepRecurse(depth - 1)
}

// capturingPanicLogger records the arguments passed to ErrorS.
type capturingPanicLogger struct {
	msg   string
	err   error
	attrs []any
}

// ErrorS records a structured error log call.
func (l *capturingPanicLogger) ErrorS(_ context.Context, msg string, err error,
	attrs ...any) {

	l.msg = msg
	l.err = err
	l.attrs = attrs
}

// panickingPanicLogger panics whenever ErrorS is called.
type panickingPanicLogger struct{}

// ErrorS simulates a panic raised while reporting another panic.
func (panickingPanicLogger) ErrorS(context.Context, string, error, ...any) {
	panic("logger failed")
}

// panicTestValue is a non-string panic value used to verify that structured
// logging preserves the original value.
type panicTestValue struct {
	code int
}
