package main

import (
	"bytes"
	"encoding/json"
	"os"

	"github.com/codegangsta/cli"
	"golang.org/x/net/context"
	"li.lan/labs/plasma/lnrpc"
)

func printRespJson(resp interface{}) {
	b, err := json.Marshal(resp)
	if err != nil {
		fatal(err)
	}

	var out bytes.Buffer
	json.Indent(&out, b, "", "\t")
	out.WriteTo(os.Stdout)
}

var ShellCommand = cli.Command{
	Name:   "shell",
	Usage:  "enter interactive shell",
	Action: shell,
}

var NewAddressCommand = cli.Command{
	Name:   "newaddress",
	Usage:  "gets the next address in the HD chain",
	Action: newAddress,
}

func newAddress(ctx *cli.Context) {
	client := getClient(ctx)

	ctxb := context.Background()
	addr, err := client.NewAddress(ctxb, &lnrpc.NewAddressRequest{})
	if err != nil {
		fatal(err)
	}

	printRespJson(addr)
}

var SendManyCommand = cli.Command{
	Name: "sendmany",
	Usage: "create and broadcast a transaction paying the specified " +
		"amount(s) to the passed address(es)",
	Action: sendMany,
}

func sendMany(ctx *cli.Context) {
	var amountToAddr map[string]int64

	jsonMap := ctx.Args().Get(0)
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		fatal(err)
	}

	ctxb := context.Background()
	client := getClient(ctx)

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{amountToAddr})
	if err != nil {
		fatal(err)
	}

	printRespJson(txid)
}
