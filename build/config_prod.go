//go:build !dev
// +build !dev

package build

// consoleLoggerCfg embeds the LoggerConfig struct along with any extensions
// specific to a production deployment.
//
//nolint:lll
type consoleLoggerCfg struct {
	LoggerConfig
}

// defaultConsoleLoggerCfg returns the default consoleLoggerCfg for the prod
// console logger.
func defaultConsoleLoggerCfg() *consoleLoggerCfg {
	return &consoleLoggerCfg{
		LoggerConfig: LoggerConfig{
			CallSite: callSiteOff,
		},
	}
}
