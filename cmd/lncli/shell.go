package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/codegangsta/cli"
	"google.golang.org/grpc"
	"li.lan/labs/plasma/lnrpc"
)

func shell(z *cli.Context) {
	fmt.Printf("LN shell v0.0FTW\n")
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
	//	if cmd == "help" {
	//		Help()
	//		return nil
	//	}

	//	if cmd == "pbx" {
	//		err = PbxConnect(args)
	//		if err != nil {
	//			fmt.Printf("pbx error: %s\n", err)
	//		}
	//		return nil
	//	}
	//	if cmd == "lnwho" {
	//		err = LnWho(args)
	//		if err != nil {
	//			fmt.Printf("LN WhoAreYou error: %s\n", err)
	//		}
	//		return nil
	//	}
	//	if cmd == "lnd" {
	//		err = LnDisconnect(args)
	//		if err != nil {
	//			fmt.Printf("LN disconnect error: %s\n", err)
	//		}
	//		return nil
	//	}
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
	//	if cmd == "lnl" {
	//		err = LnListen(args)
	//		if err != nil {
	//			fmt.Printf("LN listen error: %s\n", err)
	//		}
	//		return nil
	//	}
	//	if cmd == "txsend" {
	//		err = TxSend(args)
	//		if err != nil {
	//			fmt.Printf("txsend error: %s\n", err)
	//		}
	//		return nil
	//	}
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

func RpcConnect(args []string) error {
	//	client := getClient(ctx)
	opts := []grpc.DialOption{grpc.WithInsecure()}
	conn, err := grpc.Dial("localhost:10000", opts...)
	if err != nil {
		return err
	}
	state, err := conn.State()
	if err != nil {
		return err
	}
	fmt.Printf("connection state: %s\n", state.String())

	lnClient := lnrpc.NewLightningClient(conn)
	//	lnClient.NewAddress(nil, nil, nil) // crashes

	err = conn.Close()
	if err != nil {
		return err
	}

	return nil
}

func LnConnect(args []string) error {
	fmt.Printf("lnconnect, %d args\n", len(args))
	return nil
}

// For testing.  Syntax: lnhi hello world
func LnChat(args []string) error {

	var chat string
	for _, s := range args {
		chat += s + " "
	}
	//	msg := append([]byte{lnwire.MSGID_TEXTCHAT}, []byte(chat)...)

	fmt.Printf("will send text message: %s\n", chat)
	return nil
}
