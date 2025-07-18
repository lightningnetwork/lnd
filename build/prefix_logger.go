package build

import (
	"context"
	"fmt"

	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
)

// PrefixLogger is a logger wrapper that adds a prefix to all log messages while
// delegating to a base logger. This allows the base logger's level to be
// changed dynamically and have it affect all PrefixLogger instances.
type PrefixLogger struct {
	prefix string
	base   btclog.Logger
}

// NewPrefixLogger creates a new PrefixLogger that adds the given prefix to all
// log messages.
func NewPrefixLogger(base btclog.Logger, prefix string) *PrefixLogger {
	return &PrefixLogger{
		prefix: prefix,
		base:   base,
	}
}

// addPrefix adds the prefix to the given format string.
func (p *PrefixLogger) addPrefix(format string) string {
	return p.prefix + " " + format
}

// Tracef logs a formatted message at the trace level.
func (p *PrefixLogger) Tracef(format string, params ...interface{}) {
	p.base.Tracef(p.addPrefix(format), params...)
}

// Debugf logs a formatted message at the debug level.
func (p *PrefixLogger) Debugf(format string, params ...interface{}) {
	p.base.Debugf(p.addPrefix(format), params...)
}

// Infof logs a formatted message at the info level.
func (p *PrefixLogger) Infof(format string, params ...interface{}) {
	p.base.Infof(p.addPrefix(format), params...)
}

// Warnf logs a formatted message at the warn level.
func (p *PrefixLogger) Warnf(format string, params ...interface{}) {
	p.base.Warnf(p.addPrefix(format), params...)
}

// Errorf logs a formatted message at the error level.
func (p *PrefixLogger) Errorf(format string, params ...interface{}) {
	p.base.Errorf(p.addPrefix(format), params...)
}

// Criticalf logs a formatted message at the critical level.
func (p *PrefixLogger) Criticalf(format string, params ...interface{}) {
	p.base.Criticalf(p.addPrefix(format), params...)
}

// Trace logs a message at the trace level.
func (p *PrefixLogger) Trace(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Trace(p.prefix + " " + msg)
}

// Debug logs a message at the debug level.
func (p *PrefixLogger) Debug(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Debug(p.prefix + " " + msg)
}

// Info logs a message at the info level.
func (p *PrefixLogger) Info(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Info(p.prefix + " " + msg)
}

// Warn logs a message at the warn level.
func (p *PrefixLogger) Warn(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Warn(p.prefix + " " + msg)
}

// Error logs a message at the error level.
func (p *PrefixLogger) Error(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Error(p.prefix + " " + msg)
}

// Critical logs a message at the critical level.
func (p *PrefixLogger) Critical(v ...interface{}) {
	msg := fmt.Sprint(v...)
	p.base.Critical(p.prefix + " " + msg)
}

// Level returns the current logging level of the base logger.
func (p *PrefixLogger) Level() btclogv1.Level {
	return p.base.Level()
}

// SetLevel sets the logging level of the base logger.
func (p *PrefixLogger) SetLevel(level btclogv1.Level) {
	p.base.SetLevel(level)
}

// SubSystem creates a new logger for a subsystem.
func (p *PrefixLogger) SubSystem(tag string) btclog.Logger {
	return p.base.SubSystem(tag)
}

// WithPrefix creates a new logger with an additional prefix.
func (p *PrefixLogger) WithPrefix(prefix string) btclog.Logger {
	// Combine the existing prefix with the new one.
	newPrefix := p.prefix + ": " + prefix
	return NewPrefixLogger(p.base, newPrefix)
}

// TraceS logs a structured message at the trace level.
func (p *PrefixLogger) TraceS(ctx context.Context, msg string, attrs ...any) {
	p.base.TraceS(ctx, p.prefix+" "+msg, attrs...)
}

// DebugS logs a structured message at the debug level.
func (p *PrefixLogger) DebugS(ctx context.Context, msg string, attrs ...any) {
	p.base.DebugS(ctx, p.prefix+" "+msg, attrs...)
}

// InfoS logs a structured message at the info level.
func (p *PrefixLogger) InfoS(ctx context.Context, msg string, attrs ...any) {
	p.base.InfoS(ctx, p.prefix+" "+msg, attrs...)
}

// WarnS logs a structured message at the warn level.
func (p *PrefixLogger) WarnS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.base.WarnS(ctx, p.prefix+" "+msg, err, attrs...)
}

// ErrorS logs a structured message at the error level.
func (p *PrefixLogger) ErrorS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.base.ErrorS(ctx, p.prefix+" "+msg, err, attrs...)
}

// CriticalS logs a structured message at the critical level.
func (p *PrefixLogger) CriticalS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.base.CriticalS(ctx, p.prefix+" "+msg, err, attrs...)
}

// Compile-time check to ensure PrefixLogger implements btclog.Logger.
var _ btclog.Logger = (*PrefixLogger)(nil)
