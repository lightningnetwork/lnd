package commands

import (
	"flag"

	"github.com/urfave/cli"
)

// helpCommand replaces the help command that the cli library registers by
// default. That default implementation always renders a command with the flat
// command help template, which has no section for subcommands. So
// "lncli help wallet" only prints the name and usage of the wallet command,
// while "lncli wallet --help" also lists every command below it. We therefore
// hand a command that has subcommands to its own help output, so both ways of
// asking for help print the same thing.
//
// NOTE: The cli library only registers the global help flag if the application
// doesn't declare a command called "help" itself. Because we do, cli.HelpFlag
// is added to the application's flags by hand, see main.go.
var helpCommand = cli.Command{
	Name:      "help",
	Aliases:   []string{"h"},
	Usage:     "Shows a list of commands or help for one command",
	ArgsUsage: "[command]",
	Action:    helpCommandAction,
}

// helpCommandAction prints the help of the command named by the first
// argument, or the help of the application itself if there is no argument.
func helpCommandAction(ctx *cli.Context) error {
	args := ctx.Args()
	if !args.Present() {
		return cli.ShowAppHelp(ctx)
	}

	// A command that has no subcommands is rendered correctly by the
	// library itself. So is a name that doesn't resolve to a command at
	// all, in which case we want the library's "no help topic" error.
	name := args.First()
	command := ctx.App.Command(name)
	if command == nil || len(command.Subcommands) == 0 {
		return cli.ShowCommandHelp(ctx, name)
	}

	// Run the command with the help flag as its only argument. The library
	// prints the subcommand help and returns before any action is
	// executed, which is what "lncli <command> --help" does as well.
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	if err := set.Parse([]string{name, "--help"}); err != nil {
		return err
	}

	return command.Run(cli.NewContext(ctx.App, set, ctx))
}
