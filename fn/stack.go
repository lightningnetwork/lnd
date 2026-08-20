package fn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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

// LogRecoveredPanic records a recovered panic in a consistent structured
// format. The logger's subsystem and the captured stack identify where the
// panic was recovered. Callers can attach domain-specific context as additional
// structured attributes.
//
// Reporting is best effort. A panic while formatting or writing the record is
// recovered so the reporting path can return normally.
func LogRecoveredPanic(ctx context.Context, logger PanicLogger,
	p Panic, extraAttrs ...any) {

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
		slog.String("stack", string(p.Stack)),
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
//	        log.Errorf("recovered: %v\n%s", p.Value, p.Stack)
//	})
//
// Wrapping it in a closure instead, as in
// `defer func() { fn.RecoverPanic(..) }()`, silently does nothing: the language
// only lets recover() stop a panic when it's called by the deferred function
// itself, so one frame further down it returns nil and the panic carries on
// unwinding.
func RecoverPanic(onPanic func(Panic)) {
	r := recover()
	if r == nil {
		return
	}

	onPanic(Panic{
		Value: r,
		Stack: TruncatePanicStack(debug.Stack()),
	})
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
