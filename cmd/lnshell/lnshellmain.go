package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/lightningnetwork/lnd/lnrpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

var z lnrpc.LightningClient
var stub context.Context

func main() {
	fmt.Printf("LNShell v0.0. \n")
	fmt.Printf("Connects to LN daemon, default on 127.0.0.1:10000.\n")
	err := shellPrompt()
	if err != nil {
		log.Fatal(err)
	}
	return
}

func shellPrompt() error {
	stub = context.Background()
	opts := []grpc.DialOption{grpc.WithInsecure()}
	conn, err := grpc.Dial("localhost:10000", opts...)
	if err != nil {
		return err
	}
	z = lnrpc.NewLightningClient(conn)

	for {
		reader := bufio.NewReaderSize(os.Stdin, 4000)
		fmt.Printf("->")
		msg, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		cmdslice := strings.Fields(msg)
		if len(cmdslice) < 1 {
			continue
		}
		fmt.Printf("entered command: %s\n", msg)
		err = Shellparse(cmdslice)
		if err != nil {
			return err
		}
	}
}

func Shellparse(cmdslice []string) error {
	var err error
	var args []string
	cmd := cmdslice[0]
	if len(cmdslice) > 1 {
		args = cmdslice[1:]
	}
	if cmd == "exit" || cmd == "quit" {
		return fmt.Errorf("User exit")
	}

	if cmd == "lnhi" {
		err = LnChat(args)
		if err != nil {
			fmt.Printf("LN chat error: %s\n", err)
		}
		return nil
	}
	if cmd == "lnc" {
		err = LnConnect(args)
		if err != nil {
			fmt.Printf("LN connect error: %s\n", err)
		}
		return nil
	}
	if cmd == "lnl" {
		err = LnListen(args)
		if err != nil {
			fmt.Printf("LN listen error: %s\n", err)
		}
		return nil
	}
	fmt.Printf("Command not recognized.\n")
	return nil
}
