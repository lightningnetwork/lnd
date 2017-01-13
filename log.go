package main

import (
	"fmt"
	"os"

	"github.com/btcsuite/btclog"
	"github.com/btcsuite/seelog"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/roasbeef/btcd/connmgr"
)

// Loggers per subsystem.  Note that backendLog is a seelog logger that all of
// the subsystem loggers route their messages to.  When adding new subsystems,
// add a reference here, to the subsystemLoggers map, and the useLogger
// function.
var (
	backendLog = seelog.Disabled
	ltndLog    = btclog.Disabled
	lnwlLog    = btclog.Disabled
	peerLog    = btclog.Disabled
	fndgLog    = btclog.Disabled
	rpcsLog    = btclog.Disabled
	srvrLog    = btclog.Disabled
	ntfnLog    = btclog.Disabled
	chdbLog    = btclog.Disabled
	hswcLog    = btclog.Disabled
	utxnLog    = btclog.Disabled
	brarLog    = btclog.Disabled
	cmgrLog    = btclog.Disabled
	crtrLog    = btclog.Disabled
)

// subsystemLoggers maps each subsystem identifier to its associated logger.
var subsystemLoggers = map[string]btclog.Logger{
	"LTND": ltndLog,
	"LNWL": lnwlLog,
	"PEER": peerLog,
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
}

// useLogger updates the logger references for subsystemID to logger.  Invalid
// subsystems are ignored.
func useLogger(subsystemID string, logger btclog.Logger) {
	if _, ok := subsystemLoggers[subsystemID]; !ok {
		return
	}
	subsystemLoggers[subsystemID] = logger

	switch subsystemID {
	case "LTND":
		ltndLog = logger

	case "LNWL":
		lnwlLog = logger
		lnwallet.UseLogger(logger)

	case "PEER":
		peerLog = logger

	case "RPCS":
		rpcsLog = logger

	case "SRVR":
		srvrLog = logger

	case "NTFN":
		ntfnLog = logger
		chainntnfs.UseLogger(logger)

	case "CHDB":
		chdbLog = logger
		channeldb.UseLogger(logger)

	case "FNDG":
		fndgLog = logger

	case "HSWC":
		hswcLog = logger

	case "UTXN":
		utxnLog = logger

	case "BRAR":
		brarLog = logger

	case "CMGR":
		cmgrLog = logger
		connmgr.UseLogger(logger)

	case "CRTR":
		crtrLog = logger
		routing.UseLogger(crtrLog)
	}
}

// initSeelogLogger initializes a new seelog logger that is used as the backend
// for all logging subsystems.
func initSeelogLogger(logFile string) {
	config := `
	<seelog type="adaptive" mininterval="2000000" maxinterval="100000000"
		critmsgcount="500" minlevel="trace">
		<outputs formatid="all">
			<console />
			<rollingfile type="size" filename="%s" maxsize="10485760" maxrolls="3" />
		</outputs>
		<formats>
			<format id="all" format="%%Time %%Date [%%LEV] %%Msg%%n" />
		</formats>
	</seelog>`
	config = fmt.Sprintf(config, logFile)

	logger, err := seelog.LoggerFromConfigAsString(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v", err)
		os.Exit(1)
	}

	backendLog = logger
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

	// Default to info if the log level is invalid.
	level, ok := btclog.LogLevelFromString(logLevel)
	if !ok {
		level = btclog.InfoLvl
	}

	// Create new logger for the subsystem if needed.
	if logger == btclog.Disabled {
		logger = btclog.NewSubsystemLogger(backendLog, subsystemID+": ")
		useLogger(subsystemID, logger)
	}
	logger.SetLevel(level)
}

// setLogLevels sets the log level for all subsystem loggers to the passed
// level. It also dynamically creates the subsystem loggers as needed, so it
// can be used to initialize the logging system.
func setLogLevels(logLevel string) {
	// Configure all subsystems with the new logging level. Dynamically
	// create loggers as needed.
	for subsystemID := range subsystemLoggers {
		setLogLevel(subsystemID, logLevel)
	}
}

// logClosure is used to provide a closure over expensive logging operations
// so don't have to be performed when the logging level doesn't warrant it.
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
