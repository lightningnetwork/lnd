package main

import (
	"encoding/json"
	"fmt"

	"github.com/codegangsta/cli"
	"golang.org/x/net/context"
	"li.lan/labs/plasma/rpcprotos"
)

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

	fmt.Println(json.Marshal(addr))
}

var SendManyCommand = cli.Command{
	Name: "sendmany",
	Usage: "create and broadcast a transaction paying the specified " +
		"amount(s) to the passed address(es)",
	Action: sendMany,
}

func sendMany(ctx *cli.Context) {
	var amountToAddr map[string]int64

	fmt.Println(ctx.Args())

	jsonMap := ctx.Args().Get(0)
	if err := json.Unmarshal([]byte(jsonMap), &amountToAddr); err != nil {
		fatal(err)
	}

	fmt.Println("map: %v", amountToAddr)

	ctxb := context.Background()
	client := getClient(ctx)

	txid, err := client.SendMany(ctxb, &lnrpc.SendManyRequest{amountToAddr})
	if err != nil {
		fatal(err)
	}

	fmt.Println(json.Marshal(txid))
}
