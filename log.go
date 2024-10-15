package lnd

import (
	"github.com/btcsuite/btcd/connmgr"
	"github.com/btcsuite/btcd/rpcclient"
	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
	"github.com/lightninglabs/neutrino"
	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/build"
	"github.com/lightningnetwork/lnd/chainntnfs"
	"github.com/lightningnetwork/lnd/chainreg"
	"github.com/lightningnetwork/lnd/chanacceptor"
	"github.com/lightningnetwork/lnd/chanbackup"
	"github.com/lightningnetwork/lnd/chanfitness"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/lightningnetwork/lnd/channelnotifier"
	"github.com/lightningnetwork/lnd/cluster"
	"github.com/lightningnetwork/lnd/contractcourt"
	"github.com/lightningnetwork/lnd/discovery"
	"github.com/lightningnetwork/lnd/funding"
	"github.com/lightningnetwork/lnd/graph"
	"github.com/lightningnetwork/lnd/healthcheck"
	"github.com/lightningnetwork/lnd/htlcswitch"
	"github.com/lightningnetwork/lnd/invoices"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/lightningnetwork/lnd/lnrpc/chainrpc"
	"github.com/lightningnetwork/lnd/lnrpc/devrpc"
	"github.com/lightningnetwork/lnd/lnrpc/invoicesrpc"
	"github.com/lightningnetwork/lnd/lnrpc/neutrinorpc"
	"github.com/lightningnetwork/lnd/lnrpc/peersrpc"
	"github.com/lightningnetwork/lnd/lnrpc/routerrpc"
	"github.com/lightningnetwork/lnd/lnrpc/signrpc"
	"github.com/lightningnetwork/lnd/lnrpc/verrpc"
	"github.com/lightningnetwork/lnd/lnrpc/walletrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/lnwallet/btcwallet"
	"github.com/lightningnetwork/lnd/lnwallet/chancloser"
	"github.com/lightningnetwork/lnd/lnwallet/chanfunding"
	"github.com/lightningnetwork/lnd/lnwallet/rpcwallet"
	"github.com/lightningnetwork/lnd/monitoring"
	"github.com/lightningnetwork/lnd/netann"
	"github.com/lightningnetwork/lnd/peer"
	"github.com/lightningnetwork/lnd/peernotifier"
	"github.com/lightningnetwork/lnd/routing"
	"github.com/lightningnetwork/lnd/routing/blindedpath"
	"github.com/lightningnetwork/lnd/rpcperms"
	"github.com/lightningnetwork/lnd/signal"
	"github.com/lightningnetwork/lnd/sweep"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower"
	"github.com/lightningnetwork/lnd/watchtower/wtclient"
)

// replaceableLogger is a thin wrapper around a logger that is used so the
// logger can be replaced easily without some black pointer magic.
type replaceableLogger struct {
	btclog.Logger
	subsystem string
}

// Loggers can not be used before the log rotator has been initialized with a
// log file. This must be performed early during application startup by
// calling InitLogRotator() on the main log writer instance in the config.
var (
	// lndPkgLoggers is a list of all lnd package level loggers that are
	// registered. They are tracked here so they can be replaced once the
	// SetupLoggers function is called with the final root logger.
	lndPkgLoggers []*replaceableLogger

	// addLndPkgLogger is a helper function that creates a new replaceable
	// main lnd package level logger and adds it to the list of loggers that
	// are replaced again later, once the final root logger is ready.
	addLndPkgLogger = func(subsystem string) *replaceableLogger {
		l := &replaceableLogger{
			Logger:    build.NewSubLogger(subsystem, nil),
			subsystem: subsystem,
		}
		lndPkgLoggers = append(lndPkgLoggers, l)
		return l
	}

	// Loggers that need to be accessible from the lnd package can be placed
	// here. Loggers that are only used in sub modules can be added directly
	// by using the addSubLogger method. We declare all loggers so we never
	// run into a nil reference if they are used early. But the SetupLoggers
	// function should always be called as soon as possible to finish
	// setting them up properly with a root logger.
	ltndLog = addLndPkgLogger("LTND")
	rpcsLog = addLndPkgLogger("RPCS")
	srvrLog = addLndPkgLogger("SRVR")
	atplLog = addLndPkgLogger("ATPL")
)

// genSubLogger creates a logger for a subsystem. We provide an instance of
// a signal.Interceptor to be able to shutdown in the case of a critical error.
func genSubLogger(root *build.SubLoggerManager,
	interceptor signal.Interceptor) func(string) btclog.Logger {

	// Create a shutdown function which will request shutdown from our
	// interceptor if it is listening.
	shutdown := func() {
		if !interceptor.Listening() {
			return
		}

		interceptor.RequestShutdown()
	}

	// Return a function which will create a sublogger from our root
	// logger without shutdown fn.
	return func(tag string) btclog.Logger {
		return root.GenSubLogger(tag, shutdown)
	}
}

// SetupLoggers initializes all package-global logger variables.
//
//nolint:lll
func SetupLoggers(root *build.SubLoggerManager, interceptor signal.Interceptor) {
	genLogger := genSubLogger(root, interceptor)

	// Now that we have the proper root logger, we can replace the
	// placeholder lnd package loggers.
	for _, l := range lndPkgLoggers {
		l.Logger = build.NewSubLogger(l.subsystem, genLogger)
		SetV1SubLogger(root, l.subsystem, l.Logger)
	}

	// Initialize loggers from packages outside of `lnd` first. The
	// packages below will overwrite the names of the loggers they import.
	// For instance, the logger in `neutrino.query` is overwritten by
	// `btcwallet.chain`, which is overwritten by `lnwallet`. To ensure the
	// overwriting works, we need to initialize the loggers here so they
	// can be overwritten later.
	AddV1SubLogger(root, "BTCN", interceptor, neutrino.UseLogger)
	AddV1SubLogger(root, "CMGR", interceptor, connmgr.UseLogger)
	AddV1SubLogger(root, "RPCC", interceptor, rpcclient.UseLogger)

	// Some of the loggers declared in the main lnd package are also used
	// in sub packages.
	signal.UseLogger(ltndLog)
	autopilot.UseLogger(atplLog)

	AddV1SubLogger(root, "LNWL", interceptor, lnwallet.UseLogger)
	AddV1SubLogger(root, "DISC", interceptor, discovery.UseLogger)
	AddV1SubLogger(root, "NTFN", interceptor, chainntnfs.UseLogger)
	AddV1SubLogger(root, "CHDB", interceptor, channeldb.UseLogger)
	AddV1SubLogger(root, "HSWC", interceptor, htlcswitch.UseLogger)
	AddV1SubLogger(root, "CNCT", interceptor, contractcourt.UseLogger)
	AddV1SubLogger(root, "UTXN", interceptor, contractcourt.UseNurseryLogger)
	AddV1SubLogger(root, "BRAR", interceptor, contractcourt.UseBreachLogger)
	AddV1SubLogger(root, "SPHX", interceptor, sphinx.UseLogger)
	AddV1SubLogger(root, "SWPR", interceptor, sweep.UseLogger)
	AddV1SubLogger(root, "SGNR", interceptor, signrpc.UseLogger)
	AddV1SubLogger(root, "WLKT", interceptor, walletrpc.UseLogger)
	AddV1SubLogger(root, "ARPC", interceptor, autopilotrpc.UseLogger)
	AddV1SubLogger(root, "NRPC", interceptor, neutrinorpc.UseLogger)
	AddV1SubLogger(root, "DRPC", interceptor, devrpc.UseLogger)
	AddV1SubLogger(root, "INVC", interceptor, invoices.UseLogger)
	AddV1SubLogger(root, "NANN", interceptor, netann.UseLogger)
	AddV1SubLogger(root, "WTWR", interceptor, watchtower.UseLogger)
	AddV1SubLogger(root, "NTFR", interceptor, chainrpc.UseLogger)
	AddV1SubLogger(root, "IRPC", interceptor, invoicesrpc.UseLogger)
	AddV1SubLogger(root, "CHNF", interceptor, channelnotifier.UseLogger)
	AddV1SubLogger(root, "CHBU", interceptor, chanbackup.UseLogger)
	AddV1SubLogger(root, "PROM", interceptor, monitoring.UseLogger)
	AddV1SubLogger(root, "WTCL", interceptor, wtclient.UseLogger)
	AddV1SubLogger(root, "PRNF", interceptor, peernotifier.UseLogger)
	AddV1SubLogger(root, "CHFD", interceptor, chanfunding.UseLogger)
	AddV1SubLogger(root, "PEER", interceptor, peer.UseLogger)
	AddV1SubLogger(root, "CHCL", interceptor, chancloser.UseLogger)

	AddV1SubLogger(root, routing.Subsystem, interceptor, routing.UseLogger)
	AddV1SubLogger(root, routerrpc.Subsystem, interceptor, routerrpc.UseLogger)
	AddV1SubLogger(root, chanfitness.Subsystem, interceptor, chanfitness.UseLogger)
	AddV1SubLogger(root, verrpc.Subsystem, interceptor, verrpc.UseLogger)
	AddV1SubLogger(root, healthcheck.Subsystem, interceptor, healthcheck.UseLogger)
	AddV1SubLogger(root, chainreg.Subsystem, interceptor, chainreg.UseLogger)
	AddV1SubLogger(root, chanacceptor.Subsystem, interceptor, chanacceptor.UseLogger)
	AddV1SubLogger(root, funding.Subsystem, interceptor, funding.UseLogger)
	AddV1SubLogger(root, cluster.Subsystem, interceptor, cluster.UseLogger)
	AddV1SubLogger(root, rpcperms.Subsystem, interceptor, rpcperms.UseLogger)
	AddV1SubLogger(root, tor.Subsystem, interceptor, tor.UseLogger)
	AddV1SubLogger(root, btcwallet.Subsystem, interceptor, btcwallet.UseLogger)
	AddV1SubLogger(root, rpcwallet.Subsystem, interceptor, rpcwallet.UseLogger)
	AddV1SubLogger(root, peersrpc.Subsystem, interceptor, peersrpc.UseLogger)
	AddV1SubLogger(root, graph.Subsystem, interceptor, graph.UseLogger)
	AddV1SubLogger(root, lncfg.Subsystem, interceptor, lncfg.UseLogger)
	AddV1SubLogger(
		root, blindedpath.Subsystem, interceptor, blindedpath.UseLogger,
	)
}

// AddV1SubLogger is a helper method to conveniently create and register the
// logger of one or more sub systems.
func AddV1SubLogger(root *build.SubLoggerManager, subsystem string,
	interceptor signal.Interceptor, useLoggers ...func(btclogv1.Logger)) {

	// genSubLogger will return a callback for creating a logger instance,
	// which we will give to the root logger.
	genLogger := genSubLogger(root, interceptor)

	// Create and register just a single logger to prevent them from
	// overwriting each other internally.
	logger := build.NewSubLogger(subsystem, genLogger)
	SetV1SubLogger(root, subsystem, logger, useLoggers...)
}

// SetV1SubLogger is a helper method to conveniently register the logger of a
// sub system. Note that the btclog v2 logger implements the btclog v1 logger
// which is why we can pass the v2 logger to the UseLogger call-backs that
// expect the v1 logger.
func SetV1SubLogger(root *build.SubLoggerManager, subsystem string,
	logger btclog.Logger, useLoggers ...func(btclogv1.Logger)) {

	root.RegisterSubLogger(subsystem, logger)
	for _, useLogger := range useLoggers {
		useLogger(logger)
	}
}
