package build

import "github.com/btcsuite/btclog"

// PrefixLog is a pass-through logger that adds a prefix to every logged line.
type PrefixLog struct {
	log    btclog.Logger
	prefix string
}

// NewPrefixLog instantiates a new prefixed logger.
func NewPrefixLog(prefix string, log btclog.Logger) *PrefixLog {
	return &PrefixLog{
		prefix: prefix,
		log:    log,
	}
}

// addFormatPrefix prepends the prefix to a format string.
func (p *PrefixLog) addFormatPrefix(s string) string {
	return p.prefix + " " + s
}

// addArgsPrefix prepends the prefix to a list of arguments.
func (p *PrefixLog) addArgsPrefix(args []interface{}) []interface{} {
	return append([]interface{}{p.prefix}, args...)
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

// Level returns the current logging level.
func (p *PrefixLog) Level() btclog.Level {
	return p.log.Level()
}

// SetLevel changes the logging level to the passed level.
func (p *PrefixLog) SetLevel(level btclog.Level) {
	p.log.SetLevel(level)
}

// Assert that PrefixLog fulfills the btclog.Logger interface.
var _ btclog.Logger = &PrefixLog{}
