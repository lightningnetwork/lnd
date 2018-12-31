package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"strconv"

	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/urfave/cli"

	"gopkg.in/macaroon.v2"
)

// accountMacaroonPermissions is the list of permissions that is given to an
// account macaroon. These permissions are enough to see the off-chain balance,
// generate invoices, list payments and pay invoices.
var accountMacaroonPermissions = []*lnrpc.MacaroonPermission{
	{
		Entity: "info",
		Action: "read",
	},
	{
		Entity: "offchain",
		Action: "read",
	},
	{
		Entity: "offchain",
		Action: "write",
	},
	{
		Entity: "invoices",
		Action: "read",
	},
	{
		Entity: "invoices",
		Action: "write",
	},
}

var accountMacaroonCommand = cli.Command{
	Name:      "accountmacaroon",
	Category:  "Macaroons",
	Usage:     "Bakes a new macaroon that is bound to an account",
	ArgsUsage: "[--save_to=] account_id",
	Description: `
	Bakes a new macaroon and binds it to the given account. Using this
	macaroon will allow the user to spend the account's balance by paying
	invoices, see their account balance and create invoices that pay to the
	account. The macaroon only contains a fixed set of permissions that
	allow exactly these use cases. All other operations will not be allowed.

	The resulting macaroon can either be shown on command line in hex
	serialized format or it can be saved directly to a file using the
	--save_to argument.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name: "save_to",
			Usage: "save the delegated macaroon to this file " +
				"using the ",
		},
		cli.StringFlag{
			Name: "account_id",
			Usage: "the ID of the account the macaroon should be " +
				"restricted to",
		},
	},
	Action: actionDecorator(accountMacaroon),
}

func accountMacaroon(ctx *cli.Context) error {
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	// Show command help if no arguments.
	if ctx.NArg() == 0 {
		return cli.ShowCommandHelp(ctx, "accountmacaroon")
	}
	args := ctx.Args()

	var (
		savePath  string
		accountID []byte
		err       error
	)

	if ctx.String("save_to") != "" {
		savePath = cleanAndExpandPath(ctx.String("save_to"))
	}

	switch {
	case ctx.IsSet("account_id"):
		accountID, err = hex.DecodeString(ctx.String("account_id"))
		if err != nil {
			return fmt.Errorf("unable to parse account ID: %v", err)
		}
	case args.Present():
		accountID, err = hex.DecodeString(args.First())
		if err != nil {
			return fmt.Errorf("unable to parse account ID: %v", err)
		}
		args = args.Tail()
	}
	if len(accountID) != macaroons.AccountIDLen {
		return fmt.Errorf("invalid account ID")
	}

	// Now we have gathered all the input we need and can do the actual
	// RPC call.
	req := &lnrpc.BakeMacaroonRequest{
		Permissions: accountMacaroonPermissions,
	}
	resp, err := client.BakeMacaroon(context.Background(), req)
	if err != nil {
		return err
	}

	// Now we should have gotten a valid macaroon. Unmarshal it so we can
	// add the first-party account ID caveat to it.
	macBytes, err := hex.DecodeString(resp.Macaroon)
	if err != nil {
		return err
	}
	unmarshalMac := &macaroon.Macaroon{}
	if err = unmarshalMac.UnmarshalBinary(macBytes); err != nil {
		return err
	}

	// Add the macaroon account constraint.
	var id macaroons.AccountID
	copy(id[:], accountID)
	constrainedMac, err := macaroons.AddConstraints(
		unmarshalMac, macaroons.AccountLockConstraint(id))
	if err != nil {
		fatal(err)
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

var createAccountCommand = cli.Command{
	Name:      "createaccount",
	Category:  "Accounts",
	Usage:     "Create a new off-chain account with a balance.",
	ArgsUsage: "balance [expiration_date]",
	Description: `
	Adds an entry to the account database. This entry represents an amount
	of satoshis (account balance) that can be spent using off-chain
	transactions (e.g. paying invoices).

	Macaroons can be created to be locked to an account. This makes sure
	that the bearer of the macaroon can only spend at most that amount of
	satoshis through the daemon that has issued the macaroon.

	Accounts only assert a maximum amount spendable. Having a certain
	account balance does not guarantee that the node has the channel
	liquidity to actually spend that amount.
	`,
	Flags: []cli.Flag{
		cli.Uint64Flag{
			Name:  "balance",
			Usage: "the initial balance of the account",
		},
		cli.Int64Flag{
			Name: "expiration_date",
			Usage: "the expiration date of the account expressed " +
				"in seconds since the unix epoch. 0 means" +
				"it does not expire. (default 0)",
		},
	},
	Action: actionDecorator(createAccount),
}

func createAccount(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var (
		initialBalance uint64
		expirationDate int64
		err            error
	)
	args := ctx.Args()

	switch {
	case ctx.IsSet("balance"):
		initialBalance = ctx.Uint64("balance")
	case args.Present():
		initialBalance, err = strconv.ParseUint(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf("unable to decode balance %v", err)
		}
		args = args.Tail()
	}

	switch {
	case ctx.IsSet("expiration_date"):
		expirationDate = ctx.Int64("expiration_date")
	case args.Present():
		expirationDate, err = strconv.ParseInt(args.First(), 10, 64)
		if err != nil {
			return fmt.Errorf(
				"unable to decode expiration_date: %v", err,
			)
		}
		args = args.Tail()
	}

	if initialBalance <= 0 {
		return fmt.Errorf("initial balance cannot be smaller than 1")
	}

	req := &lnrpc.CreateAccountRequest{
		AccountBalance: initialBalance,
		ExpirationDate: expirationDate,
	}
	resp, err := client.CreateAccount(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var listAccountsCommand = cli.Command{
	Name:     "listaccounts",
	Category: "Accounts",
	Usage:    "Lists all off-chain accounts.",
	Description: `
	Returns all accounts that are currently stored in the account
	database.
	`,
	Action: actionDecorator(listAccounts),
}

func listAccounts(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	req := &lnrpc.ListAccountsRequest{}
	resp, err := client.ListAccounts(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var removeAccountCommand = cli.Command{
	Name:      "removeaccount",
	Category:  "Accounts",
	Usage:     "Removes an off-chain account from the database.",
	ArgsUsage: "id",
	Description: `
	Removes an account entry from the account database.
	`,
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "id",
			Usage: "the ID of the account",
		},
	},
	Action: actionDecorator(removeAccount),
}

func removeAccount(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getClient(ctx)
	defer cleanUp()

	var accountID string
	args := ctx.Args()

	switch {
	case ctx.IsSet("id"):
		accountID = ctx.String("id")
	case args.Present():
		accountID = args.First()
		args = args.Tail()
	default:
		return fmt.Errorf("id argument missing")
	}

	if len(accountID) == 0 {
		return fmt.Errorf("id argument missing")
	}
	if _, err := hex.DecodeString(accountID); err != nil {
		return err
	}

	req := &lnrpc.RemoveAccountRequest{
		Id: accountID,
	}
	_, err := client.RemoveAccount(ctxb, req)
	return err
}
