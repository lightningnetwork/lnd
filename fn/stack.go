package fn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
)

const (
	// maxPanicStackSize is the largest recovered-panic stack trace returned
	// by these helpers. Bounding it keeps repeated reports from consuming
	// excessive log space.
	maxPanicStackSize = 8192

	// panicStackTruncatedMsg is appended in place of whatever we cut, so
	// that whoever reads the log can tell the trace is incomplete instead
	// of assuming it ended where it stops.
	panicStackTruncatedMsg = "\n... stack trace truncated ..."
)

// errRecoveredPanic is the sentinel error attached to every structured log
// record emitted for a recovered panic.
var errRecoveredPanic = errors.New("recovered panic")

// Panic carries the details of a panic we stopped: the value handed to panic(),
// and a stack trace bounded to a size that's safe to log. The trace still
// covers the frames the panic came from, not just the point we caught it at.
type Panic struct {
	// Value is whatever was passed to panic().
	Value any

	// Stack is the stack trace of the panicking goroutine, truncated to a
	// bounded size.
	Stack []byte
}

// PanicLogger is the minimal structured-logging surface needed to report a
// recovered panic. Keeping the interface here avoids coupling fn to a concrete
// logging package.
type PanicLogger interface {
	ErrorS(context.Context, string, error, ...any)
}

// PanicDebugLogger extends PanicLogger with the structured debug logging used
// when a remotely-triggerable path should keep its stack trace below error
// level.
type PanicDebugLogger interface {
	PanicLogger

	DebugS(context.Context, string, ...any)
}

// LogRecoveredPanic records a recovered panic in a consistent structured
// format. The logger's subsystem and the captured stack identify where the
// panic was recovered. Callers can attach domain-specific context as additional
// structured attributes.
//
// Reporting is best effort. A panic while formatting or writing the record is
// recovered so the reporting path can return normally.
func LogRecoveredPanic(ctx context.Context, logger PanicLogger,
	p Panic, extraAttrs ...any) {

	logRecoveredPanic(ctx, logger, p, true, extraAttrs...)
}

// LogRecoveredPanicWithDebugStack records the panic summary at error level and
// its bounded stack trace at debug level. This is intended for paths where an
// untrusted caller could repeatedly trigger the same panic and amplify error
// logs. Domain-specific attributes are included in both records so they can be
// correlated.
//
// Reporting is best effort. A panic while formatting or writing either record
// is recovered so the reporting path can return normally.
func LogRecoveredPanicWithDebugStack(ctx context.Context,
	logger PanicDebugLogger, p Panic, extraAttrs ...any) {

	logRecoveredPanic(ctx, logger, p, false, extraAttrs...)

	if logger == nil {
		return
	}

	// A panic in the logging path must not interrupt the caller's panic
	// containment and cleanup.
	defer func() {
		_ = recover()
	}()

	attrs := make([]any, 0, len(extraAttrs)+1)
	attrs = append(attrs, extraAttrs...)
	attrs = append(attrs, slog.String("stack", string(p.Stack)))

	logger.DebugS(ctx, "Recovered panic stack trace", attrs...)
}

// logRecoveredPanic records the common error-level panic fields and optionally
// includes the stack trace in the same record.
func logRecoveredPanic(ctx context.Context, logger PanicLogger, p Panic,
	includeStack bool, extraAttrs ...any) {

	if logger == nil {
		return
	}

	// A panic in the logging path must not interrupt the caller's panic
	// containment and cleanup.
	defer func() {
		_ = recover()
	}()

	attrs := []any{
		slog.String("panic_type", fmt.Sprintf("%T", p.Value)),
		slog.Any("panic_value", p.Value),
	}
	if includeStack {
		attrs = append(attrs, slog.String("stack", string(p.Stack)))
	}
	attrs = append(attrs, extraAttrs...)

	logger.ErrorS(
		ctx, "Recovered panic", errRecoveredPanic, attrs...,
	)
}

// RecoverPanic stops a panic from unwinding the calling goroutine any further
// and hands the details to onPanic. If the goroutine isn't panicking, onPanic
// is never called and no stack is captured.
//
// This MUST be deferred directly:
//
//	defer fn.RecoverPanic(func(p fn.Panic) {
//	        fn.LogRecoveredPanic(ctx, logger, p)
//	        // ...subsystem-specific cleanup...
//	})
//
// Wrapping it in a closure instead, as in
// `defer func() { fn.RecoverPanic(..) }()`, silently does nothing: the language
// only lets recover() stop a panic when it's called by the deferred function
// itself, so one frame further down it returns nil and the panic carries on
// unwinding.
//
// After recovery, the function containing the defer returns. Execution does
// not resume at the panic site. Place the defer in a per-item wrapper only when
// its caller can safely continue with the next item.
//
// A nil onPanic callback is allowed. If the callback panics, that new panic is
// also contained and reported best-effort to stderr because the normal logging
// path may be what failed.
func RecoverPanic(onPanic func(Panic)) {
	recoverPanic(recover(), onPanic, os.Stderr)
}

// recoverPanic handles a value returned by recover. Keeping the callback and
// fallback handling separate makes the containment behavior testable without
// replacing the process-wide stderr stream.
func recoverPanic(r any, onPanic func(Panic), fallbackWriter io.Writer) {
	if r == nil {
		return
	}

	if onPanic == nil {
		return
	}

	panicDetails := Panic{
		Value: r,
		Stack: TruncatePanicStack(debug.Stack()),
	}

	defer func() {
		callbackPanic := recover()
		if callbackPanic == nil {
			return
		}

		reportCallbackPanic(
			fallbackWriter, r, callbackPanic,
			TruncatePanicStack(debug.Stack()),
		)
	}()

	onPanic(panicDetails)
}

// reportCallbackPanic writes a last-resort report when the callback used to
// contain a panic also panics. The report path must never propagate a panic.
func reportCallbackPanic(writer io.Writer, originalPanic,
	callbackPanic any, stack []byte) {

	defer func() {
		_ = recover()
	}()

	_, _ = fmt.Fprintf(
		writer, "fn: panic handler panicked: %v "+
			"(while containing %T: %v)\n%s\n", callbackPanic,
		originalPanic, originalPanic, stack,
	)
}

// PanicStack returns the stack trace of the calling goroutine, bounded to the
// configured size.
func PanicStack() []byte {
	return TruncatePanicStack(debug.Stack())
}

// TruncatePanicStack caps a panic stack trace at a bounded size. Where it can,
// it cuts on a line boundary so the last frame in the log stays readable rather
// than ending mid-token, and it marks the cut so the trace doesn't look
// complete.
func TruncatePanicStack(stack []byte) []byte {
	if len(stack) <= maxPanicStackSize {
		return stack
	}

	suffix := []byte(panicStackTruncatedMsg)
	maxStackLen := maxPanicStackSize - len(suffix)
	searchStack := stack[:maxStackLen+1]
	newLineIndex := bytes.LastIndexByte(searchStack, '\n')
	if newLineIndex > 0 {
		maxStackLen = newLineIndex
	}

	truncatedStack := make([]byte, 0, maxStackLen+len(suffix))
	truncatedStack = append(truncatedStack, stack[:maxStackLen]...)
	truncatedStack = append(truncatedStack, suffix...)

	return truncatedStack
}
