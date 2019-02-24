package main

import (
	"bufio"
	"flag"
	"strings"
	"testing"

	"github.com/urfave/cli"
)

func TestValidatePassword(t *testing.T) {
	testPasswords := map[string]bool{
		"abc":          false,
		"abc123":       false,
		"testpassword": true,
		"1234567":      false,
		"12345678":     true,
	}

	for password, valid := range testPasswords {
		err := validatePassword([]byte(password))
		if err != nil && valid == true {
			t.Fatalf("expected password %s to be valid but got error %s", password, err)
		}
	}

}

func getAppContext() *cli.Context {
	app := cli.NewApp()
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:  "rpcserver",
			Value: defaultRPCHostPort,
			Usage: "host:port of ln daemon",
		},
		cli.StringFlag{
			Name:  "lnddir",
			Value: defaultLndDir,
			Usage: "path to lnd's base directory",
		},
		cli.StringFlag{
			Name:  "tlscertpath",
			Value: defaultTLSCertPath,
			Usage: "path to TLS certificate",
		},
		cli.StringFlag{
			Name:  "chain, c",
			Usage: "the chain lnd is running on e.g. bitcoin",
			Value: "bitcoin",
		},
		cli.StringFlag{
			Name: "network, n",
			Usage: "the network lnd is running on e.g. mainnet, " +
				"testnet, etc.",
			Value: "mainnet",
		},
		cli.BoolTFlag{
			Name:  "no-macaroons",
			Usage: "disable macaroon authentication",
		},
		cli.StringFlag{
			Name:  "macaroonpath",
			Usage: "path to macaroon file",
		},
		cli.Int64Flag{
			Name:  "macaroontimeout",
			Value: 60,
			Usage: "anti-replay macaroon validity time in seconds",
		},
		cli.StringFlag{
			Name:  "macaroonip",
			Usage: "if set, lock macaroon to specific IP address",
		},
	}
	return cli.NewContext(app, getFlagSet(app.Flags), nil)
}

type mockTerminalReader struct {
	stdInChan      chan []byte
	signalChan     chan struct{}
	stdInCallCount int
}

func (t *mockTerminalReader) ReadPassword() ([]byte, error) {
	t.signalChan <- struct{}{}
	return <-t.stdInChan, nil
}

func (t *mockTerminalReader) ReadStdin() *bufio.Reader {
	t.signalChan <- struct{}{}
	return bufio.NewReader(strings.NewReader(string(<-t.stdInChan)))
}

func getFlagSet(flags []cli.Flag) *flag.FlagSet {
	set := flag.NewFlagSet("lncli", flag.ContinueOnError)

	for _, f := range flags {
		f.Apply(set)
	}
	return set
}

func TestCreateWithGoodPassword(t *testing.T) {
	ctx := getAppContext()
	terminalReader := &mockTerminalReader{
		stdInChan:  make(chan []byte, 1),
		signalChan: make(chan struct{}, 1),
	}
	go func() {
	Loop:
		for {
			select {
			case <-terminalReader.signalChan:
				terminalReader.stdInCallCount++
				if terminalReader.stdInCallCount == 1 || terminalReader.stdInCallCount == 2 {
					terminalReader.stdInChan <- []byte("testpasswordwithgoodlength")
				} else if terminalReader.stdInCallCount == 3 {
					terminalReader.stdInChan <- []byte("n\n")
				} else if terminalReader.stdInCallCount == 4 || terminalReader.stdInCallCount == 5 {
					terminalReader.stdInChan <- []byte("password123")
				} else if terminalReader.stdInCallCount == 6 {
					break Loop
				}
			}
		}

	}()
	err := create(ctx, terminalReader)
	if !strings.Contains(err.Error(), "all SubConns") {
		t.Fatalf("Error: %s", err)
	}
}

func TestCreateWithBadPassword(t *testing.T) {
	password := "shortpw"
	ctx := getAppContext()
	terminalReader := &mockTerminalReader{
		stdInChan:  make(chan []byte, 1),
		signalChan: make(chan struct{}, 1),
	}
	go func() {
		<-terminalReader.signalChan
		terminalReader.stdInChan <- []byte(password)
	}()
	err := create(ctx, terminalReader)
	if err == nil || !strings.Contains(err.Error(), "password must have at least 8 characters") {
		t.Fatalf("%s should be an invalid password but returned valid", password)
	}
}
