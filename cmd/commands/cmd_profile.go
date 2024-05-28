package commands

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/urfave/cli"
	"gopkg.in/macaroon.v2"
)

var (
	// defaultLncliDir is the default directory to store the profile file
	// in. This defaults to:
	//   C:\Users\<username>\AppData\Local\Lncli\ on Windows
	//   ~/.lncli/ on Linux
	//   ~/Library/Application Support/Lncli/ on MacOS
	defaultLncliDir = btcutil.AppDataDir("lncli", false)

	// defaultProfileFile is the full, absolute path of the profile file.
	defaultProfileFile = path.Join(defaultLncliDir, "profiles.json")
)

var profileSubCommand = cli.Command{
	Name:     "profile",
	Category: "Profiles",
	Usage:    "Create and manage lncli profiles.",
	Description: `
	Profiles for lncli are an easy and comfortable way to manage multiple
	nodes from the command line by storing node specific parameters like RPC
	host, network, TLS certificate path or macaroons in a named profile.

	To use a predefined profile, just use the '--profile=myprofile' (or
	short version '-p=myprofile') with any lncli command.

	A default profile can also be defined, lncli will then always use the
	connection/node parameters from that profile instead of the default
	values.

	WARNING: Setting a default profile changes the default behavior of
	lncli! To disable the use of the default profile for a single command,
	set '--profile= '.

	The profiles are stored in a file called profiles.json in the user's
	home directory, for example:
		C:\Users\<username>\AppData\Local\Lncli\profiles.json on Windows
		~/.lncli/profiles.json on Linux
		~/Library/Application Support/Lncli/profiles.json on MacOS
	`,
	Subcommands: []cli.Command{
		profileListCommand,
		profileAddCommand,
		profileRemoveCommand,
		profileSetDefaultCommand,
		profileUnsetDefaultCommand,
		profileAddMacaroonCommand,
	},
}

var profileListCommand = cli.Command{
	Name:   "list",
	Usage:  "Lists all lncli profiles",
	Action: profileList,
}

func profileList(_ *cli.Context) error {
	f, err := loadProfileFile(defaultProfileFile)
	if err != nil {
		return err
	}

	printJSON(f)
	return nil
}

var profileAddCommand = cli.Command{
	Name:      "add",
	Usage:     "Add a new profile.",
	ArgsUsage: "name",
	Description: `
	Add a new named profile to the main profiles.json. All global options
	(see 'lncli --help') passed into this command are stored in that named
	profile.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "name",
			Usage: "the name of the new profile",
		},
		cli.BoolFlag{
			Name:  "default",
			Usage: "set the new profile to be the default profile",
		},
	},
	Action: profileAdd,
}

func profileAdd(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "add")
	}

	// Load the default profile file or create a new one if it doesn't exist
	// yet.
	f, err := loadProfileFile(defaultProfileFile)
	switch {
	case err == errNoProfileFile:
		f = &profileFile{}
		_ = os.MkdirAll(path.Dir(defaultProfileFile), 0700)

	case err != nil:
		return err
	}

	// Create a profile struct from all the global options.
	profile, err := profileFromContext(ctx, true, false)
	if err != nil {
		return fmt.Errorf("could not load global options: %w", err)
	}

	// Finally, all that's left is to get the profile name from either
	// positional argument or flag.
	args := ctx.Args()
	switch {
	case ctx.IsSet("name"):
		profile.Name = ctx.String("name")
	case args.Present():
		profile.Name = args.First()
	default:
		return fmt.Errorf("name argument missing")
	}

	// Is there already a profile with that name?
	for _, p := range f.Profiles {
		if p.Name == profile.Name {
			return fmt.Errorf("a profile with the name %s already "+
				"exists", profile.Name)
		}
	}

	// Do we need to update the default entry to be this one?
	if ctx.Bool("default") {
		f.Default = profile.Name
	}

	// All done, store the updated profile file.
	f.Profiles = append(f.Profiles, profile)
	if err = saveProfileFile(defaultProfileFile, f); err != nil {
		return fmt.Errorf("error writing profile file %s: %w",
			defaultProfileFile, err)
	}

	fmt.Printf("Profile %s added to file %s.\n", profile.Name,
		defaultProfileFile)
	return nil
}

var profileRemoveCommand = cli.Command{
	Name:        "remove",
	Usage:       "Remove a profile",
	ArgsUsage:   "name",
	Description: `Remove the specified profile from the profile file.`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "name",
			Usage: "the name of the profile to delete",
		},
	},
	Action: profileRemove,
}

func profileRemove(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "remove")
	}

	// Load the default profile file.
	f, err := loadProfileFile(defaultProfileFile)
	if err != nil {
		return fmt.Errorf("could not load profile file: %w", err)
	}

	// Get the profile name from either positional argument or flag.
	var (
		args  = ctx.Args()
		name  string
		found = false
	)
	switch {
	case ctx.IsSet("name"):
		name = ctx.String("name")
	case args.Present():
		name = args.First()
	default:
		return fmt.Errorf("name argument missing")
	}

	if len(f.Profiles) == 0 {
		return fmt.Errorf("there are no existing profiles")
	}

	// Create a copy of all profiles but don't include the one to delete.
	newProfiles := make([]*profileEntry, 0, len(f.Profiles)-1)
	for _, p := range f.Profiles {
		// Skip the one we want to delete.
		if p.Name == name {
			found = true

			if p.Name == f.Default {
				fmt.Println("Warning: removing default profile.")
			}
			continue
		}

		// Keep all others.
		newProfiles = append(newProfiles, p)
	}

	// If what we were looking for didn't exist in the first place, there's
	// no need for updating the file.
	if !found {
		return fmt.Errorf("profile with name %s not found in file",
			name)
	}

	// Great, everything updated, now let's save the file.
	f.Profiles = newProfiles
	return saveProfileFile(defaultProfileFile, f)
}

var profileSetDefaultCommand = cli.Command{
	Name:      "setdefault",
	Usage:     "Set the default profile.",
	ArgsUsage: "name",
	Description: `
	Set a specified profile to be used as the default profile.

	WARNING: Setting a default profile changes the default behavior of
	lncli! To disable the use of the default profile for a single command,
	set '--profile= '.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "name",
			Usage: "the name of the profile to set as default",
		},
	},
	Action: profileSetDefault,
}

func profileSetDefault(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "setdefault")
	}

	// Load the default profile file.
	f, err := loadProfileFile(defaultProfileFile)
	if err != nil {
		return fmt.Errorf("could not load profile file: %w", err)
	}

	// Get the profile name from either positional argument or flag.
	var (
		args  = ctx.Args()
		name  string
		found = false
	)
	switch {
	case ctx.IsSet("name"):
		name = ctx.String("name")
	case args.Present():
		name = args.First()
	default:
		return fmt.Errorf("name argument missing")
	}

	// Make sure the new default profile actually exists.
	for _, p := range f.Profiles {
		if p.Name == name {
			found = true
			f.Default = p.Name

			break
		}
	}

	// If the default profile doesn't exist, there's no need for updating
	// the file.
	if !found {
		return fmt.Errorf("profile with name %s not found in file",
			name)
	}

	// Great, everything updated, now let's save the file.
	return saveProfileFile(defaultProfileFile, f)
}

var profileUnsetDefaultCommand = cli.Command{
	Name:  "unsetdefault",
	Usage: "Unsets the default profile.",
	Description: `
	Disables the use of a default profile and restores lncli to its original
	behavior.
	`,
	Action: profileUnsetDefault,
}

func profileUnsetDefault(_ *cli.Context) error {
	// Load the default profile file.
	f, err := loadProfileFile(defaultProfileFile)
	if err != nil {
		return fmt.Errorf("could not load profile file: %w", err)
	}

	// Save the file with the flag disabled.
	f.Default = ""
	return saveProfileFile(defaultProfileFile, f)
}

var profileAddMacaroonCommand = cli.Command{
	Name:      "addmacaroon",
	Usage:     "Add a macaroon to a profile's macaroon jar.",
	ArgsUsage: "macaroon-name",
	Description: `
	Add an additional macaroon specified by the global option --macaroonpath
	to an existing profile's macaroon jar.

	If no profile is selected, the macaroon is added to the default profile
	(if one exists). To add a macaroon to a specific profile, use the global
	--profile=myprofile option.

	If multiple macaroons exist in a profile's macaroon jar, the one to use
	can be specified with the global option --macfromjar=xyz.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "name",
			Usage: "the name of the macaroon",
		},
		cli.BoolFlag{
			Name: "default",
			Usage: "set the new macaroon to be the default " +
				"macaroon in the jar",
		},
	},
	Action: profileAddMacaroon,
}

func profileAddMacaroon(ctx *cli.Context) error {
	if ctx.NArg() == 0 && ctx.NumFlags() == 0 {
		return cli.ShowCommandHelp(ctx, "addmacaroon")
	}

	// Load the default profile file or create a new one if it doesn't exist
	// yet.
	f, err := loadProfileFile(defaultProfileFile)
	if err != nil {
		return fmt.Errorf("could not load profile file: %w", err)
	}

	// Finally, all that's left is to get the profile name from either
	// positional argument or flag.
	var (
		args        = ctx.Args()
		profileName string
		macName     string
	)
	switch {
	case ctx.IsSet("name"):
		macName = ctx.String("name")
	case args.Present():
		macName = args.First()
	default:
		return fmt.Errorf("name argument missing")
	}

	// Make sure the user actually set a macaroon path to use.
	if !ctx.GlobalIsSet("macaroonpath") {
		return fmt.Errorf("macaroonpath global option missing")
	}

	// Find out which profile we should add the macaroon. The global flag
	// takes precedence over the default profile.
	if f.Default != "" {
		profileName = f.Default
	}
	if ctx.GlobalIsSet("profile") {
		profileName = ctx.GlobalString("profile")
	}
	if len(strings.TrimSpace(profileName)) == 0 {
		return fmt.Errorf("no profile specified and no default " +
			"profile exists")
	}

	// Is there a profile with that name?
	var selectedProfile *profileEntry
	for _, p := range f.Profiles {
		if p.Name == profileName {
			selectedProfile = p
			break
		}
	}
	if selectedProfile == nil {
		return fmt.Errorf("profile with name %s not found", profileName)
	}

	// Does a macaroon with that name already exist?
	for _, m := range selectedProfile.Macaroons.Jar {
		if m.Name == macName {
			return fmt.Errorf("a macaroon with the name %s "+
				"already exists", macName)
		}
	}

	// Do we need to update the default entry to be this one?
	if ctx.Bool("default") {
		selectedProfile.Macaroons.Default = macName
	}

	// Now load and possibly encrypt the macaroon file.
	macPath := lncfg.CleanAndExpandPath(ctx.GlobalString("macaroonpath"))
	macBytes, err := os.ReadFile(macPath)
	if err != nil {
		return fmt.Errorf("unable to read macaroon path: %w", err)
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return fmt.Errorf("unable to decode macaroon: %w", err)
	}
	macEntry := &macaroonEntry{
		Name: macName,
	}
	if err = macEntry.storeMacaroon(mac, nil); err != nil {
		return fmt.Errorf("unable to store macaroon: %w", err)
	}

	// All done, store the updated profile file.
	selectedProfile.Macaroons.Jar = append(
		selectedProfile.Macaroons.Jar, macEntry,
	)
	if err = saveProfileFile(defaultProfileFile, f); err != nil {
		return fmt.Errorf("error writing profile file %s: %w",
			defaultProfileFile, err)
	}

	fmt.Printf("Macaroon %s added to profile %s in file %s.\n", macName,
		selectedProfile.Name, defaultProfileFile)
	return nil
}
