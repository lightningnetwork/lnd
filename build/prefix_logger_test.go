package build

import (
	"bytes"
	"strings"
	"testing"

	btclogv1 "github.com/btcsuite/btclog"
	"github.com/btcsuite/btclog/v2"
	"github.com/stretchr/testify/require"
)

// TestPrefixLoggerLevelChange tests that PrefixLogger correctly delegates to
// the base logger and respects dynamic level changes.
func TestPrefixLoggerLevelChange(t *testing.T) {
	t.Parallel()

	// We'll user this buffer to capture the log output.
	var buf bytes.Buffer

	// Create a base logger with a handler that writes to our buffer.
	handler := btclog.NewDefaultHandler(&buf)
	handler.SetLevel(btclogv1.LevelDebug)
	baseLogger := btclog.NewSLogger(handler)

	// Verify that WithPrefix creates independent logger instances.
	t.Run("WithPrefix creates independent loggers", func(t *testing.T) {
		// Create a logger with WithPrefix (simulating the old
		// behavior).
		withPrefixLogger := baseLogger.WithPrefix("Peer1")

		// Create a PrefixLogger (our new implementation).
		prefixLogger := NewPrefixLogger(baseLogger, "Peer2")

		buf.Reset()

		// Both should log debug messages initially.
		withPrefixLogger.Debugf("test debug message")
		require.Contains(t, buf.String(), "test debug message")
		require.Contains(t, buf.String(), "Peer1")

		buf.Reset()

		prefixLogger.Debugf("test debug message")
		require.Contains(t, buf.String(), "test debug message")
		require.Contains(t, buf.String(), "Peer2")

		// Now we'll change base logger level to INFO.
		baseLogger.SetLevel(btclogv1.LevelInfo)

		buf.Reset()

		// WithPrefix logger still logs debug: this was the original bug
		// we've set out to fix with PrefixLogger!
		//
		// This will still log because WithPrefix created an independent
		// logger.
		withPrefixLogger.Debugf("debug after level change")

		require.Contains(t, buf.String(), "debug after level change")

		buf.Reset()

		// PrefixLogger respects the base logger's new level. This shows
		// that the loggers are independent.
		//
		// This should NOT log because PrefixLogger delegates to base
		// logger.
		prefixLogger.Debugf("debug after level change")
		require.Empty(t, buf.String())

		buf.Reset()

		// Both should still log INFO messages.
		withPrefixLogger.Infof("info message")
		require.Contains(t, buf.String(), "info message")

		buf.Reset()

		prefixLogger.Infof("info message")
		require.Contains(t, buf.String(), "info message")
	})

	// Verify that PrefixLogger correctly adds prefixes.
	t.Run("PrefixLogger adds prefixes correctly", func(t *testing.T) {
		baseLogger.SetLevel(btclogv1.LevelDebug)
		prefixLogger := NewPrefixLogger(baseLogger, "TestPeer")

		buf.Reset()

		// Test basic formatting first.
		prefixLogger.Debugf("formatted %s", "message")
		output := buf.String()
		require.Contains(t, output, "TestPeer formatted message")

		// Next, we'll test unformatted messages.
		buf.Reset()
		prefixLogger.Info("unformatted message")
		output = buf.String()
		require.Contains(t, output, "TestPeer unformatted message")
	})

	// Verify that nested prefixes work correctly.
	t.Run("Nested prefixes work correctly", func(t *testing.T) {
		baseLogger.SetLevel(btclogv1.LevelDebug)
		prefixLogger1 := NewPrefixLogger(baseLogger, "Peer1")
		prefixLogger2 := prefixLogger1.WithPrefix("Channel1")

		buf.Reset()

		prefixLogger2.Infof("nested prefix test")

		output := buf.String()
		require.Contains(
			t, output, "Peer1: Channel1 nested prefix test",
		)
	})

	// Verify that the Level() method returns the base logger's level.
	t.Run("Level method returns current level", func(t *testing.T) {
		prefixLogger := NewPrefixLogger(baseLogger, "TestPeer")

		baseLogger.SetLevel(btclogv1.LevelWarn)
		require.Equal(t, btclogv1.LevelWarn, prefixLogger.Level())

		baseLogger.SetLevel(btclogv1.LevelDebug)
		require.Equal(t, btclogv1.LevelDebug, prefixLogger.Level())
	})
}

// TestPrefixLoggerSimulatesPeerBehavior simulates the actual peer logging
// behavior to demonstrate the fix.
func TestPrefixLoggerSimulatesPeerBehavior(t *testing.T) {
	var buf bytes.Buffer

	// Simulate the global peerLog.
	handler := btclog.NewDefaultHandler(&buf)
	handler.SetLevel(btclogv1.LevelDebug)
	peerLog := btclog.NewSLogger(handler)

	// Simulate creating multiple peers (like NewBrontide does).
	peer1Logger := NewPrefixLogger(peerLog, "Peer(abc123)")
	peer2Logger := NewPrefixLogger(peerLog, "Peer(def456)")

	buf.Reset()

	// Initially, debug messages are logged.
	peer1Logger.Debugf("Received ChannelUpdate")
	peer2Logger.Debugf("Received ChannelUpdate")

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "Peer(abc123) Received ChannelUpdate")
	require.Contains(t, lines[1], "Peer(def456) Received ChannelUpdate")

	// Simulate running "lncli debuglevel --level info".
	peerLog.SetLevel(btclogv1.LevelInfo)

	buf.Reset()

	// Now debug messages should NOT be logged.
	peer1Logger.Debugf("Received ChannelUpdate")
	peer2Logger.Debugf("Received ChannelUpdate")
	require.Empty(
		t, buf.String(), "debug messages should "+
			"not be logged at INFO level",
	)

	buf.Reset()

	// But INFO messages should still be logged.
	peer1Logger.Infof("Peer connected")
	peer2Logger.Infof("Peer connected")

	output = buf.String()
	lines = strings.Split(strings.TrimSpace(output), "\n")
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "Peer(abc123) Peer connected")
	require.Contains(t, lines[1], "Peer(def456) Peer connected")
}
