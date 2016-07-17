package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"text/template"

	"github.com/roasbeef/btcutil"
)

var (
	numRandBytes = flag.Int("num_rand_bytes", 32, "Number of random bytes to read for both the username and password")
)

const (
	autoRpcTemplate = "[Application Options]\nrpcuser={{.Username}}\nrpcpass={{.Password}}"
)

type basicRpcOptions struct {
	Username string
	Password string
}

func randBase64string(numBytes int) string {
	randBuf := make([]byte, numBytes)
	if _, err := rand.Read(randBuf); err != nil {
		log.Fatalf("unable to read random bytes: %v", err)
	}
	return base64.StdEncoding.EncodeToString(randBuf)
}

func main() {
	fmt.Println("Creating random rpc config for btcd")
	t := template.Must(template.New("rpcOptions").Parse(autoRpcTemplate))

	randRpcOptions := basicRpcOptions{
		Username: randBase64string(*numRandBytes),
		Password: randBase64string(*numRandBytes),
	}

	var autoAuth bytes.Buffer
	if err := t.Execute(&autoAuth, randRpcOptions); err != nil {
		log.Fatalf("unable to generate random auth: %v")
	}

	btcdHomeDir := btcutil.AppDataDir("btcd", false)
	btcctlHomeDir := btcutil.AppDataDir("btcctl", false)
	btcdConfigPath := fmt.Sprintf("%s/btcd.conf", btcdHomeDir)
	btcctlConfigPath := fmt.Sprintf("%s/btcctl.conf", btcctlHomeDir)

	if err := ioutil.WriteFile(btcdConfigPath, autoAuth.Bytes(), 0644); err != nil {
		log.Fatalf("unable to write config for btcd: %v", err)
	}

	if err := ioutil.WriteFile(btcctlConfigPath, autoAuth.Bytes(), 0644); err != nil {
		log.Fatalf("unable to write config for btcctl: %v", err)
	}
	fmt.Println("fin.")
}
