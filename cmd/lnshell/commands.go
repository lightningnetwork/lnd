package main

import (
	"fmt"
	"time"

	"google.golang.org/grpc"
	"li.lan/labs/plasma/lnrpc"
)

// connects via grpc to the ln node.  default (hardcoded?) local:10K
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
	time.Sleep(time.Second * 2)
	//	lnClient := lnrpc.NewLightningClient(conn)
	//	lnClient.NewAddress(nil, nil, nil) // crashes

	state, err = conn.State()
	if err != nil {
		return err
	}
	fmt.Printf("connection state: %s\n", state.String())

	err = conn.Close()
	if err != nil {
		return err
	}
	return nil
}

func LnConnect(args []string) error {
	//	var err error
	if len(args) == 0 {
		return fmt.Errorf("need: lnc pubkeyhash@hostname or pkh (via pbx)")
	}

	req := new(lnrpc.LNConnectRequest)
	req.IdAtHost = args[0]
	resp, err := z.LNConnect(stub, req)
	if err != nil {
		return err
	}
	fmt.Printf("connected.  remote lnid is %x\n", resp.LnID)
	return nil
}

// LnListen listens on the default port for incoming connections
func LnListen(args []string) error {

	req := new(lnrpc.TCPListenRequest)
	req.Hostport = "0.0.0.0:2448"
	_, err := z.TCPListen(stub, req)
	if err != nil {
		return err
	}

	fmt.Printf("started TCP port listener\n")
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
	req := new(lnrpc.LnChatRequest)
	req.DestID = []byte("testID")
	req.Msg = chat
	_, err := z.LNChat(stub, req)
	if err != nil {
		return err
	}
	fmt.Printf("got response but there's nothing in it\n")
	return nil
}
