package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"net"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/urfave/cli"
	"gopkg.in/macaroon.v2"
)

var bakeMacaroonCommand = cli.Command{
	Name:     "bakemacaroon",
	Category: "Macaroons",
	Usage: "Bakes a new macaroon with the provided list of permissions " +
		"and restrictions",
	ArgsUsage: "[--save_to=] [--timeout=] [--ip_address=] permissions...",
	Description: `
	Bake a new macaroon that grants the provided permissions and
	optionally adds restrictions (timeout, IP address) to it.

	The new macaroon can either be shown on command line in hex serialized
	format or it can be saved directly to a file using the --save_to
	argument.

	A permission is a tuple of an entity and an action, separated by a
	colon. Multiple operations can be added as arguments, for example:

	lncli bakemacaroon info:read invoices:write foo:bar
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "save_to",
			Usage: "save the created macaroon to this file " +
				"using the default binary format",
		},
		cli.Uint64Flag{
			Name: "timeout",
			Usage: "the number of seconds the macaroon will be " +
				"valid before it times out",
		},
		cli.StringFlag{
			Name:  "ip_address",
			Usage: "the IP address the macaroon will be bound to",
		},
	},
	Action: actionDecorator(bakeMacaroon),
}

func bakeMacaroon(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments.
	if ctx.NArg() == 0 {
		return cli.ShowCommandHelp(ctx, "bakemacaroon")
	}
	args := ctx.Args()

	var (
		savePath          string
		timeout           int64
		ipAddress         net.IP
		parsedPermissions []*lnrpc.MacaroonPermission
		err               error
	)

	if ctx.String("save_to") != "" {
		savePath = cleanAndExpandPath(ctx.String("save_to"))
	}

	if ctx.IsSet("timeout") {
		timeout = ctx.Int64("timeout")
		if timeout <= 0 {
			return fmt.Errorf("timeout must be greater than 0")
		}
	}

	if ctx.IsSet("ip_address") {
		ipAddress = net.ParseIP(ctx.String("ip_address"))
		if ipAddress == nil {
			return fmt.Errorf("unable to parse ip_address: %s",
				ctx.String("ip_address"))
		}
	}

	// A command line argument can't be an empty string. So we'll check each
	// entry if it's a valid entity:action tuple. The content itself is
	// validated server side. We just make sure we can parse it correctly.
	for _, permission := range args {
		tuple := strings.Split(permission, ":")
		if len(tuple) != 2 {
			return fmt.Errorf("unable to parse "+
				"permission tuple: %s", permission)
		}
		entity, action := tuple[0], tuple[1]
		if entity == "" {
			return fmt.Errorf("invalid permission [%s]. entity "+
				"cannot be empty", permission)
		}
		if action == "" {
			return fmt.Errorf("invalid permission [%s]. action "+
				"cannot be empty", permission)
		}

		// No we can assume that we have a formally valid entity:action
		// tuple. The rest of the validation happens server side.
		parsedPermissions = append(
			parsedPermissions, &lnrpc.MacaroonPermission{
				Entity: entity,
				Action: action,
			},
		)
	}

	// Now we have gathered all the input we need and can do the actual
	// RPC call.
	req := &lnrpc.BakeMacaroonRequest{
		Permissions: parsedPermissions,
	}
	resp, err := client.BakeMacaroon(context.Background(), req)
	if err != nil {
		return err
	}

	// Now we should have gotten a valid macaroon. Unmarshal it so we can
	// add first-party caveats (if necessary) to it.
	macBytes, err := hex.DecodeString(resp.Macaroon)
	if err != nil {
		return err
	}
	unmarshalMac := &macaroon.Macaroon{}
	if err = unmarshalMac.UnmarshalBinary(macBytes); err != nil {
		return err
	}

	// Now apply the desired constraints to the macaroon. This will always
	// create a new macaroon object, even if no constraints are added.
	macConstraints := make([]macaroons.Constraint, 0)
	if timeout > 0 {
		macConstraints = append(
			macConstraints, macaroons.TimeoutConstraint(timeout),
		)
	}
	if ipAddress != nil {
		macConstraints = append(
			macConstraints,
			macaroons.IPLockConstraint(ipAddress.String()),
		)
	}
	constrainedMac, err := macaroons.AddConstraints(
		unmarshalMac, macConstraints...,
	)
	if err != nil {
		return err
	}
	macBytes, err = constrainedMac.MarshalBinary()
	if err != nil {
		return err
	}

	// Now we can output the result. We either write it binary serialized to
	// a file or write to the standard output using hex encoding.
	switch {
	case savePath != "":
		err = ioutil.WriteFile(savePath, macBytes, 0644)
		if err != nil {
			return err
		}
		fmt.Printf("Macaroon saved to %s\n", savePath)

	default:
		fmt.Printf("%s\n", hex.EncodeToString(macBytes))
	}

	return nil
}
