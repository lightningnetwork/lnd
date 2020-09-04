package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"path"
	"strings"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"github.com/urfave/cli"
	"gopkg.in/macaroon.v2"
)

var (
	errNoProfileFile = errors.New("no profile file found")
)

// profileEntry is a struct that represents all settings for one specific
// profile.
type profileEntry struct {
	Name        string       `json:"name"`
	RPCServer   string       `json:"rpcserver"`
	LndDir      string       `json:"lnddir"`
	Chain       string       `json:"chain"`
	Network     string       `json:"network"`
	NoMacaroons bool         `json:"no-macaroons,omitempty"`
	TLSCert     string       `json:"tlscert"`
	Macaroons   *macaroonJar `json:"macaroons"`
}

// cert returns the profile's TLS certificate as a x509 certificate pool.
func (e *profileEntry) cert() (*x509.CertPool, error) {
	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM([]byte(e.TLSCert)) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}
	return cp, nil
}

// profileFromContext creates an ephemeral profile entry from the global options
// set in the CLI context.
func profileFromContext(ctx *cli.Context, store bool) (*profileEntry, error) {
	// Parse the paths of the cert and macaroon. This will validate the
	// chain and network value as well.
	tlsCertPath, macPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, err
	}

	// Load the certificate file now. We store it as plain PEM directly.
	tlsCert, err := ioutil.ReadFile(tlsCertPath)
	if err != nil {
		return nil, fmt.Errorf("could not load TLS cert file %s: %v",
			tlsCertPath, err)
	}

	// Now load and possibly encrypt the macaroon file.
	macBytes, err := ioutil.ReadFile(macPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon path (check "+
			"the network setting!): %v", err)
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %v", err)
	}

	var pw []byte
	if store {
		// Read a password from the terminal. If it's empty, we won't
		// encrypt the macaroon and store it plaintext.
		pw, err = capturePassword(
			"Enter password to encrypt macaroon with or leave "+
				"blank to store in plaintext: ", true,
			walletunlocker.ValidatePassword,
		)
		if err != nil {
			return nil, fmt.Errorf("unable to get encryption "+
				"password: %v", err)
		}
	}
	macEntry := &macaroonEntry{}
	if err = macEntry.storeMacaroon(mac, pw); err != nil {
		return nil, fmt.Errorf("unable to store macaroon: %v", err)
	}

	// We determine the name of the macaroon from the file itself but cut
	// off the ".macaroon" at the end.
	macEntry.Name = path.Base(macPath)
	if path.Ext(macEntry.Name) == "macaroon" {
		macEntry.Name = strings.TrimSuffix(macEntry.Name, ".macaroon")
	}

	// Now that we have the complicated arguments behind us, let's return
	// the new entry with all the values populated.
	return &profileEntry{
		RPCServer:   ctx.GlobalString("rpcserver"),
		LndDir:      lncfg.CleanAndExpandPath(ctx.GlobalString("lnddir")),
		Chain:       ctx.GlobalString("chain"),
		Network:     ctx.GlobalString("network"),
		NoMacaroons: ctx.GlobalBool("no-macaroons"),
		TLSCert:     string(tlsCert),
		Macaroons: &macaroonJar{
			Default: macEntry.Name,
			Timeout: ctx.GlobalInt64("macaroontimeout"),
			IP:      ctx.GlobalString("macaroonip"),
			Jar:     []*macaroonEntry{macEntry},
		},
	}, nil
}

// loadProfileFile tries to load the file specified and JSON deserialize it into
// the profile file struct.
func loadProfileFile(file string) (*profileFile, error) {
	if !lnrpc.FileExists(file) {
		return nil, errNoProfileFile
	}

	content, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("could not load profile file %s: %v",
			file, err)
	}
	f := &profileFile{}
	err = f.unmarshalJSON(content)
	if err != nil {
		return nil, fmt.Errorf("could not unmarshal profile file %s: "+
			"%v", file, err)
	}
	return f, nil
}

// saveProfileFile stores the given profile file struct in the specified file,
// overwriting it if it already existed.
func saveProfileFile(file string, f *profileFile) error {
	content, err := f.marshalJSON()
	if err != nil {
		return fmt.Errorf("could not marshal profile: %v", err)
	}
	return ioutil.WriteFile(file, content, 0644)
}

// profileFile is a struct that represents the whole content of a profile file.
type profileFile struct {
	Default  string          `json:"default,omitempty"`
	Profiles []*profileEntry `json:"profiles"`
}

// unmarshalJSON tries to parse the given JSON and unmarshal it into the
// receiving instance.
func (f *profileFile) unmarshalJSON(content []byte) error {
	return json.Unmarshal(content, f)
}

// marshalJSON serializes the receiving instance to formatted/indented JSON.
func (f *profileFile) marshalJSON() ([]byte, error) {
	b, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("error JSON marshalling profile: %v",
			err)
	}

	var out bytes.Buffer
	err = json.Indent(&out, b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error indenting profile JSON: %v", err)
	}
	out.WriteString("\n")
	return out.Bytes(), nil
}
