package commands

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli"
)

// newHelpTestApp returns an application that is wired up the same way lncli
// is: a command that has subcommands, a command that has none, our own help
// command and the help flag that comes with it.
func newHelpTestApp(w io.Writer) *cli.App {
	app := cli.NewApp()
	app.Name = "lncli"
	app.HelpName = "lncli"
	app.Version = "1.0.0"
	app.Writer = w
	app.Flags = []cli.Flag{cli.HelpFlag}
	app.Commands = []cli.Command{
		{
			Name:     "wallet",
			Usage:    "Interact with the wallet.",
			Category: "Wallet",
			Subcommands: []cli.Command{
				{
					Name:  "accounts",
					Usage: "Interact with wallet accounts.",
				},
				{
					Name:  "labeltx",
					Usage: "Adds a label to a transaction.",
				},
			},
		},
		{
			Name:  "sendcoins",
			Usage: "Send bitcoin on-chain to an address.",
		},
		helpCommand,
	}

	return app
}

// runHelpTestApp runs the given arguments against a fresh test application and
// returns everything it printed.
func runHelpTestApp(t *testing.T, args ...string) string {
	t.Helper()

	var buf bytes.Buffer
	app := newHelpTestApp(&buf)
	require.NoError(t, app.Run(append([]string{"lncli"}, args...)))

	return buf.String()
}

// TestHelpCommandWithSubCommands makes sure that asking for the help of a
// command that has subcommands lists them, and that it prints exactly what the
// command's own help flag prints.
func TestHelpCommandWithSubCommands(t *testing.T) {
	t.Parallel()

	help := runHelpTestApp(t, "help", "wallet")
	require.Contains(t, help, "lncli wallet - Interact with the wallet.")
	require.Contains(t, help, "COMMANDS:")
	require.Contains(t, help, "accounts")
	require.Contains(t, help, "labeltx")

	require.Equal(t, runHelpTestApp(t, "wallet", "--help"), help)
}

// TestHelpCommandWithoutSubCommands makes sure the help of a command that has
// no subcommands is left alone.
func TestHelpCommandWithoutSubCommands(t *testing.T) {
	t.Parallel()

	help := runHelpTestApp(t, "help", "sendcoins")
	require.Contains(
		t, help, "lncli sendcoins - Send bitcoin on-chain to an "+
			"address.",
	)
	require.NotContains(t, help, "COMMANDS:")
}

// TestHelpCommandWithoutArguments makes sure that the help command without an
// argument still shows the help of the application itself.
func TestHelpCommandWithoutArguments(t *testing.T) {
	t.Parallel()

	help := runHelpTestApp(t, "help")
	require.Contains(t, help, "COMMANDS:")
	require.Contains(t, help, "wallet")
	require.Contains(t, help, "sendcoins")
}

// TestHelpFlag makes sure the global help flag keeps working next to our own
// help command. The cli library stops adding that flag as soon as the
// application declares a command called "help", so it has to be registered by
// hand.
func TestHelpFlag(t *testing.T) {
	t.Parallel()

	help := runHelpTestApp(t, "--help")
	require.Contains(t, help, "GLOBAL OPTIONS:")
	require.Contains(t, help, "--help, -h")
}
