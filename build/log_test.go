package build_test

import (
	"testing"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
	"github.com/stretchr/testify/require"
)

type mockSubLogger struct {
	globalLogLevel string
	subLogLevels   map[string]string
}

func (m *mockSubLogger) SubLoggers() build.SubLoggers {
	return build.SubLoggers{
		"PEER": btclog.Disabled,
		"SRVR": btclog.Disabled,
	}
}

func (m *mockSubLogger) SupportedSubsystems() []string {
	return nil
}

func (m *mockSubLogger) SetLogLevel(subsystemID string, logLevel string) {
	m.subLogLevels[subsystemID] = logLevel
}

func (m *mockSubLogger) SetLogLevels(logLevel string) {
	m.globalLogLevel = logLevel
}

// TestParseAndSetDebugLevels tests that we can properly set the log levels for
// all andspecified subsystems.
func TestParseAndSetDebugLevels(t *testing.T) {
	testCases := []struct {
		name         string
		debugLevel   string
		expErr       string
		expGlobal    string
		expSubLevels map[string]string
	}{
		{
			name:       "empty log level",
			debugLevel: "",
			expErr:     "invalid",
		},
		{
			name:       "invalid global debug level",
			debugLevel: "ddddddebug",
			expErr:     "invalid",
		},
		{
			name:       "global debug level",
			debugLevel: "debug",
			expGlobal:  "debug",
		},
		{
			name:       "invalid global debug level#2",
			debugLevel: "debug,info",
			expErr:     "invalid",
		},
		{
			name:       "invalid subsystem debug level",
			debugLevel: "AAAA=debug",
			expErr:     "invalid",
		},
		{
			name:       "valid subsystem debug level",
			debugLevel: "PEER=info,SRVR=debug",
			expSubLevels: map[string]string{
				"PEER": "info",
				"SRVR": "debug",
			},
		},
		{
			name:       "valid global+subsystem debug level",
			debugLevel: "trace,PEER=info,SRVR=debug",
			expGlobal:  "trace",
			expSubLevels: map[string]string{
				"PEER": "info",
				"SRVR": "debug",
			},
		},
		{
			name:       "invalid global+subsystem debug level",
			debugLevel: "PEER=info,debug,SRVR=debug",
			expErr:     "invalid",
		},
	}

	for _, test := range testCases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			m := &mockSubLogger{
				subLogLevels: make(map[string]string),
			}

			// If the subsystem map is empty, make and empty one to ensure
			// the equal test later succeeds.
			if len(test.expSubLevels) == 0 {
				test.expSubLevels = make(map[string]string)
			}

			err := build.ParseAndSetDebugLevels(test.debugLevel, m)
			if test.expErr != "" {
				require.Contains(t, err.Error(), test.expErr)
				return
			}
			require.NoError(t, err)

			require.Equal(t, test.expGlobal, m.globalLogLevel)
			require.Equal(t, test.expSubLevels, m.subLogLevels)
		})
	}
}
