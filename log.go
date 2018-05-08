package main

import (
	"os"

	"io"

	"fmt"
	"path/filepath"

	"github.com/btcsuite/btclog"
	"github.com/jrick/logrotate/rotator"
	"github.com/lightninglabs/neutrino"
	"github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/connmgr"
)

// logWriter implements an io.Writer that outputs to both standard output and
// the write-end pipe of an initialized log rotator.
type logWriter struct{}

func (logWriter) Write(p []byte) (n int, err error) {
	os.Stdout.Write(p)
	logRotatorPipe.Write(p)
	return len(p), nil
}

// Loggers per subsystem.  A single backend logger is created and all subsystem
// loggers created from it will write to the backend.  When adding new
// subsystems, add the subsystem logger variable here and to the
// subsystemLoggers map.
//
// Loggers can not be used before the log rotator has been initialized with a
// log file.  This must be performed early during application startup by
// calling initLogRotator.
var (
	// backendLog is the logging backend used to create all subsystem
	// loggers.  The backend must not be used before the log rotator has
	// been initialized, or data races and/or nil pointer dereferences will
	// occur.
	backendLog = btclog.NewBackend(logWriter{})

	// logRotator is one of the logging outputs.  It should be closed on
	// application shutdown.
	logRotator *rotator.Rotator

	// logRotatorPipe is the write-end pipe for writing to the log rotator.
	// It is written to by the Write method of the logWriter type.
	logRotatorPipe *io.PipeWriter

	ltndLog = backendLog.Logger("LTND")
	lnwlLog = backendLog.Logger("LNWL")
	peerLog = backendLog.Logger("PEER")
	discLog = backendLog.Logger("DISC")
	rpcsLog = backendLog.Logger("RPCS")
	srvrLog = backendLog.Logger("SRVR")
	ntfnLog = backendLog.Logger("NTFN")
	chdbLog = backendLog.Logger("CHDB")
	fndgLog = backendLog.Logger("FNDG")
	hswcLog = backendLog.Logger("HSWC")
	utxnLog = backendLog.Logger("UTXN")
	brarLog = backendLog.Logger("BRAR")
	cmgrLog = backendLog.Logger("CMGR")
	crtrLog = backendLog.Logger("CRTR")
	btcnLog = backendLog.Logger("BTCN")
	atplLog = backendLog.Logger("ATPL")
	cnctLog = backendLog.Logger("CNCT")
	sphxLog = backendLog.Logger("SPHX")
)

// Initialize package-global logger variables.
func init() {
	lnwallet.UseLogger(lnwlLog)
	discovery.UseLogger(discLog)
	chainntnfs.UseLogger(ntfnLog)
	channeldb.UseLogger(chdbLog)
	htlcswitch.UseLogger(hswcLog)
	connmgr.UseLogger(cmgrLog)
	routing.UseLogger(crtrLog)
	neutrino.UseLogger(btcnLog)
	autopilot.UseLogger(atplLog)
	contractcourt.UseLogger(cnctLog)
	sphinx.UseLogger(sphxLog)
}

// subsystemLoggers maps each subsystem identifier to its associated logger.
var subsystemLoggers = map[string]btclog.Logger{
	"LTND": ltndLog,
	"LNWL": lnwlLog,
	"PEER": peerLog,
	"DISC": discLog,
	"RPCS": rpcsLog,
	"SRVR": srvrLog,
	"NTFN": ntfnLog,
	"CHDB": chdbLog,
	"FNDG": fndgLog,
	"HSWC": hswcLog,
	"UTXN": utxnLog,
	"BRAR": brarLog,
	"CMGR": cmgrLog,
	"CRTR": crtrLog,
	"BTCN": btcnLog,
	"ATPL": atplLog,
	"CNCT": cnctLog,
	"SPHX": sphxLog,
}

// initLogRotator initializes the logging rotator to write logs to logFile and
// create roll files in the same directory.  It must be called before the
// package-global log rotator variables are used.
func initLogRotator(logFile string, MaxLogFileSize int, MaxLogFiles int) {
	logDir, _ := filepath.Split(logFile)
	err := os.MkdirAll(logDir, 0700)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create log directory: %v\n", err)
		os.Exit(1)
	}
	r, err := rotator.New(logFile, int64(MaxLogFileSize*1024), false, MaxLogFiles)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create file rotator: %v\n", err)
		os.Exit(1)
	}

	pr, pw := io.Pipe()
	go r.Run(pr)

	logRotator = r
	logRotatorPipe = pw
}

// setLogLevel sets the logging level for provided subsystem.  Invalid
// subsystems are ignored.  Uninitialized subsystems are dynamically created as
// needed.
func setLogLevel(subsystemID string, logLevel string) {
	// Ignore invalid subsystems.
	logger, ok := subsystemLoggers[subsystemID]
	if !ok {
		return
	}

	// Defaults to info if the log level is invalid.
	level, _ := btclog.LevelFromString(logLevel)
	logger.SetLevel(level)
}

// setLogLevels sets the log level for all subsystem loggers to the passed
// level. It also dynamically creates the subsystem loggers as needed, so it
// can be used to initialize the logging system.
func setLogLevels(logLevel string) {
	// Configure all sub-systems with the new logging level.  Dynamically
	// create loggers as needed.
	for subsystemID := range subsystemLoggers {
		setLogLevel(subsystemID, logLevel)
	}
}

// logClosure is used to provide a closure over expensive logging operations so
// don't have to be performed when the logging level doesn't warrant it.
type logClosure func() string

// String invokes the underlying function and returns the result.
func (c logClosure) String() string {
	return c()
}

// newLogClosure returns a new closure over a function that returns a string
// which itself provides a Stringer interface so that it can be used with the
// logging system.
func newLogClosure(c func() string) logClosure {
	return logClosure(c)
}
