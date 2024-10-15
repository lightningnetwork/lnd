package build

import (
	"context"

	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
)

// PrefixLog is a pass-through logger that adds a prefix to every logged line.
type PrefixLog struct {
	log    btclog.Logger
	prefix string
	attr   []any
}

// NewPrefixLog instantiates a new prefixed logger.
func NewPrefixLog(prefix string, log btclog.Logger, attrs ...any) *PrefixLog {
	return &PrefixLog{
		log:    log,
		prefix: prefix,
		attr:   attrs,
	}
}

// addFormatPrefix prepends the prefix to a format string.
func (p *PrefixLog) addFormatPrefix(s string) string {
	return p.prefix + " " + s
}

// addArgsPrefix prepends the prefix to a list of arguments.
func (p *PrefixLog) addArgsPrefix(args []any) []any {
	return append([]interface{}{p.prefix}, args...)
}

// mergeAttr merges the given set of attributes with any attributes that the
// logger was initialised with.
func (p *PrefixLog) mergeAttr(attrs []any) []any {
	return append(append([]any{}, attrs...), p.attr...)
}

// Tracef formats message according to format specifier and writes to to log
// with LevelTrace.
func (p *PrefixLog) Tracef(format string, params ...interface{}) {
	p.log.Tracef(p.addFormatPrefix(format), params...)
}

// Debugf formats message according to format specifier and writes to log with
// LevelDebug.
func (p *PrefixLog) Debugf(format string, params ...interface{}) {
	p.log.Debugf(p.addFormatPrefix(format), params...)
}

// Infof formats message according to format specifier and writes to log with
// LevelInfo.
func (p *PrefixLog) Infof(format string, params ...interface{}) {
	p.log.Infof(p.addFormatPrefix(format), params...)
}

// Warnf formats message according to format specifier and writes to to log with
// LevelWarn.
func (p *PrefixLog) Warnf(format string, params ...interface{}) {
	p.log.Warnf(p.addFormatPrefix(format), params...)
}

// Errorf formats message according to format specifier and writes to to log
// with LevelError.
func (p *PrefixLog) Errorf(format string, params ...interface{}) {
	p.log.Errorf(p.addFormatPrefix(format), params...)
}

// Criticalf formats message according to format specifier and writes to log
// with LevelCritical.
func (p *PrefixLog) Criticalf(format string, params ...interface{}) {
	p.log.Criticalf(p.addFormatPrefix(format), params...)
}

// Trace formats message using the default formats for its operands and writes
// to log with LevelTrace.
func (p *PrefixLog) Trace(v ...interface{}) {
	p.log.Trace(p.addArgsPrefix(v)...)
}

// Debug formats message using the default formats for its operands and writes
// to log with LevelDebug.
func (p *PrefixLog) Debug(v ...interface{}) {
	p.log.Debug(p.addArgsPrefix(v)...)
}

// Info formats message using the default formats for its operands and writes to
// log with LevelInfo.
func (p *PrefixLog) Info(v ...interface{}) {
	p.log.Info(p.addArgsPrefix(v)...)
}

// Warn formats message using the default formats for its operands and writes to
// log with LevelWarn.
func (p *PrefixLog) Warn(v ...interface{}) {
	p.log.Warn(p.addArgsPrefix(v)...)
}

// Error formats message using the default formats for its operands and writes
// to log with LevelError.
func (p *PrefixLog) Error(v ...interface{}) {
	p.log.Error(p.addArgsPrefix(v)...)
}

// Critical formats message using the default formats for its operands and
// writes to log with LevelCritical.
func (p *PrefixLog) Critical(v ...interface{}) {
	p.log.Critical(p.addArgsPrefix(v)...)
}

// TraceS writes a structured log with the given message and key-value pair
// attributes with LevelTrace to the log.
func (p *PrefixLog) TraceS(ctx context.Context, msg string, attrs ...any) {
	p.log.TraceS(ctx, p.addFormatPrefix(msg), p.mergeAttr(attrs)...)
}

// DebugS writes a structured log with the given message and key-value pair
// attributes with LevelDebug to the log.
func (p *PrefixLog) DebugS(ctx context.Context, msg string, attrs ...any) {
	p.log.DebugS(ctx, p.addFormatPrefix(msg), p.mergeAttr(attrs)...)
}

// InfoS writes a structured log with the given message and key-value pair
// attributes with LevelInfo to the log.
func (p *PrefixLog) InfoS(ctx context.Context, msg string, attrs ...any) {
	p.log.InfoS(ctx, p.addFormatPrefix(msg), p.mergeAttr(attrs)...)
}

// WarnS writes a structured log with the given message and key-value pair
// attributes with LevelWarn to the log.
func (p *PrefixLog) WarnS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.log.WarnS(ctx, p.addFormatPrefix(msg), err, p.mergeAttr(attrs)...)
}

// ErrorS writes a structured log with the given message and key-value pair
// attributes with LevelError to the log.
func (p *PrefixLog) ErrorS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.log.ErrorS(ctx, p.addFormatPrefix(msg), err, p.mergeAttr(attrs)...)
}

// CriticalS writes a structured log with the given message and key-value pair
// attributes with LevelCritical to the log.
func (p *PrefixLog) CriticalS(ctx context.Context, msg string, err error,
	attrs ...any) {

	p.log.CriticalS(ctx, p.addFormatPrefix(msg), err, p.mergeAttr(attrs)...)
}

// Level returns the current logging level.
func (p *PrefixLog) Level() btclogv1.Level {
	return p.log.Level()
}

// SetLevel changes the logging level to the passed level.
func (p *PrefixLog) SetLevel(level btclogv1.Level) {
	p.log.SetLevel(level)
}

// Assert that PrefixLog fulfills the btclog.Logger interface.
var _ btclog.Logger = &PrefixLog{}
