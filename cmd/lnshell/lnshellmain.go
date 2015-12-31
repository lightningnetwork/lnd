package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
)

func main() {
	fmt.Printf("LNShell v0.0. \n")
	fmt.Printf("Connects to LN daemon, default on 127.0.0.1:10000.\n")
	shellPrompt()
	return
}

func shellPrompt() {
	for {
		reader := bufio.NewReaderSize(os.Stdin, 4000)
		fmt.Printf("->")
		msg, err := reader.ReadString('\n')
		if err != nil {
			log.Fatal(err)
		}
		cmdslice := strings.Fields(msg)
		if len(cmdslice) < 1 {
			continue
		}
		fmt.Printf("entered command: %s\n", msg)
		err = Shellparse(cmdslice)
		if err != nil {
			log.Fatal(err)
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

	if cmd == "rpc" {
		err = RpcConnect(args)
		if err != nil {
			fmt.Printf("RPC connect error: %s\n", err)
		}
		return nil
	}

	fmt.Printf("Command not recognized.\n")
	return nil
}
