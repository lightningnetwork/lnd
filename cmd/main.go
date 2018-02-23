package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	sphinx "github.com/lightningnetwork/lightning-onion"
	"github.com/roasbeef/btcd/btcec"
	"github.com/roasbeef/btcd/chaincfg"
)

// main implements a simple command line utility that can be used in order to
// either generate a fresh mix-header or decode and fully process an existing
// one given a private key.
func main() {
	args := os.Args

	assocData := bytes.Repeat([]byte{'B'}, 32)

	if len(args) == 1 {
		fmt.Printf("Usage: %s (generate|decode) <private-keys>\n", args[0])
	} else if args[1] == "generate" {
		var privKeys []*btcec.PrivateKey
		var route []*btcec.PublicKey
		for i, hexKey := range args[2:] {
			binKey, err := hex.DecodeString(hexKey)
			if err != nil || len(binKey) != 32 {
				log.Fatalf("%s is not a valid hex privkey %s", hexKey, err)
			}
			privkey, pubkey := btcec.PrivKeyFromBytes(btcec.S256(), binKey)
			route = append(route, pubkey)
			privKeys = append(privKeys, privkey)
			fmt.Fprintf(os.Stderr, "Node %d pubkey %x\n", i, pubkey.SerializeCompressed())
		}

		sessionKey, _ := btcec.PrivKeyFromBytes(btcec.S256(), bytes.Repeat([]byte{'A'}, 32))

		var hopsData []sphinx.HopData
		for i := 0; i < len(route); i++ {
			hopsData = append(hopsData, sphinx.HopData{
				Realm:         0x00,
				ForwardAmount: uint32(i),
				OutgoingCltv:  uint32(i),
			})
			copy(hopsData[i].NextAddress[:], bytes.Repeat([]byte{byte(i)}, 8))
		}

		msg, err := sphinx.NewOnionPacket(route, sessionKey, hopsData, assocData)
		if err != nil {
			log.Fatalf("Error creating message: %v", err)
		}

		w := bytes.NewBuffer([]byte{})
		err = msg.Encode(w)

		if err != nil {
			log.Fatalf("Error serializing message: %v", err)
		}

		fmt.Printf("%x\n", w.Bytes())
	} else if args[1] == "decode" {
		binKey, err := hex.DecodeString(args[2])
		if len(binKey) != 32 || err != nil {
			log.Fatalf("Argument not a valid hex private key")
		}

		hexBytes, _ := ioutil.ReadAll(os.Stdin)
		binMsg, err := hex.DecodeString(strings.TrimSpace(string(hexBytes)))
		if err != nil {
			log.Fatalf("Error decoding message: %s", err)
		}

		privkey, _ := btcec.PrivKeyFromBytes(btcec.S256(), binKey)
		s := sphinx.NewRouter(privkey, &chaincfg.TestNet3Params, nil)

		var packet sphinx.OnionPacket
		err = packet.Decode(bytes.NewBuffer(binMsg))

		if err != nil {
			log.Fatalf("Error parsing message: %v", err)
		}
		p, err := s.ProcessOnionPacket(&packet, assocData)
		if err != nil {
			log.Fatalf("Failed to decode message: %s", err)
		}

		w := bytes.NewBuffer([]byte{})
		err = p.Packet.Encode(w)

		if err != nil {
			log.Fatalf("Error serializing message: %v", err)
		}
		fmt.Printf("%x\n", w.Bytes())
	}
}
