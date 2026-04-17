package commands

import (
	"testing"
	"time"

	"github.com/lightningnetwork/lnd/routing"
	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

func TestSetCfgCommandFlagDefaults(t *testing.T) {
	t.Parallel()

	aprioriDefaults := routing.DefaultAprioriConfig()
	bimodalDefaults := routing.DefaultBimodalConfig()

	require.Equal(t, uint(routing.DefaultMaxMcHistory),
		findUintFlag(t, setCfgCommand.Flags, "pmtnr").Value)
	require.Equal(t, routing.DefaultMinFailureRelaxInterval,
		findDurationFlag(t, setCfgCommand.Flags, "failrelax").Value)
	require.Equal(t, aprioriDefaults.PenaltyHalfLife,
		findDurationFlag(t, setCfgCommand.Flags, "apriorihalflife").Value)
	require.Equal(t, aprioriDefaults.AprioriHopProbability,
		findFloat64Flag(t, setCfgCommand.Flags, "apriorihopprob").Value)
	require.Equal(t, aprioriDefaults.AprioriWeight,
		findFloat64Flag(t, setCfgCommand.Flags, "aprioriweight").Value)
	require.Equal(t, aprioriDefaults.CapacityFraction,
		findFloat64Flag(t, setCfgCommand.Flags, "aprioricapacityfraction").Value)
	require.Equal(t, bimodalDefaults.BimodalDecayTime,
		findDurationFlag(t, setCfgCommand.Flags, "bimodaldecaytime").Value)
	require.Equal(t, uint64(bimodalDefaults.BimodalScaleMsat),
		findUint64Flag(t, setCfgCommand.Flags, "bimodalscale").Value)
	require.Equal(t, bimodalDefaults.BimodalNodeWeight,
		findFloat64Flag(t, setCfgCommand.Flags, "bimodalweight").Value)
}

func findUintFlag(t *testing.T, flags []cli.Flag, name string) cli.UintFlag {
	t.Helper()

	for _, flag := range flags {
		if typed, ok := flag.(cli.UintFlag); ok && typed.Name == name {
			return typed
		}
	}

	t.Fatalf("uint flag %q not found", name)
	return cli.UintFlag{}
}

func findUint64Flag(t *testing.T, flags []cli.Flag, name string) cli.Uint64Flag {
	t.Helper()

	for _, flag := range flags {
		if typed, ok := flag.(cli.Uint64Flag); ok && typed.Name == name {
			return typed
		}
	}

	t.Fatalf("uint64 flag %q not found", name)
	return cli.Uint64Flag{}
}

func findFloat64Flag(t *testing.T, flags []cli.Flag, name string) cli.Float64Flag {
	t.Helper()

	for _, flag := range flags {
		if typed, ok := flag.(cli.Float64Flag); ok && typed.Name == name {
			return typed
		}
	}

	t.Fatalf("float64 flag %q not found", name)
	return cli.Float64Flag{}
}

func findDurationFlag(t *testing.T, flags []cli.Flag, name string) cli.DurationFlag {
	t.Helper()

	for _, flag := range flags {
		if typed, ok := flag.(cli.DurationFlag); ok && typed.Name == name {
			return typed
		}
	}

	t.Fatalf("duration flag %q not found", name)
	return cli.DurationFlag{Value: time.Duration(0)}
}
