package commands

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	Name        string            `json:"name"`
	RPCServer   string            `json:"rpcserver"`
	LndDir      string            `json:"lnddir"`
	Network     string            `json:"network"`
	NoMacaroons bool              `json:"no-macaroons,omitempty"` // nolint:tagliatelle
	TLSCert     string            `json:"tlscert"`
	Macaroons   *macaroonJar      `json:"macaroons"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Insecure    bool              `json:"insecure,omitempty"`
}

// cert returns the profile's TLS certificate as a x509 certificate pool.
func (e *profileEntry) cert() (*x509.CertPool, error) {
	if e.TLSCert == "" {
		return nil, nil
	}

	cp := x509.NewCertPool()
	if !cp.AppendCertsFromPEM([]byte(e.TLSCert)) {
		return nil, fmt.Errorf("credentials: failed to append " +
			"certificate")
	}
	return cp, nil
}

// getGlobalOptions returns the global connection options. If a profile file
// exists, these global options might be read from a predefined profile. If no
// profile exists, the global options from the command line are returned as an
// ephemeral profile entry.
func getGlobalOptions(ctx *cli.Context, skipMacaroons bool) (*profileEntry,
	error) {

	var profileName string

	// Try to load the default profile file and depending on its existence
	// what profile to use.
	f, err := loadProfileFile(defaultProfileFile)
	switch {
	// The legacy case where no profile file exists and the user also didn't
	// request to use one. We only consider the global options here.
	case err == errNoProfileFile && !ctx.GlobalIsSet("profile"):
		return profileFromContext(ctx, false, skipMacaroons)

	// The file doesn't exist but the user specified an explicit profile.
	case err == errNoProfileFile && ctx.GlobalIsSet("profile"):
		return nil, fmt.Errorf("profile file %s does not exist",
			defaultProfileFile)

	// There is a file but we couldn't read/parse it.
	case err != nil:
		return nil, fmt.Errorf("could not read profile file %s: "+
			"%v", defaultProfileFile, err)

	// The user explicitly disabled the use of profiles for this command by
	// setting the flag to an empty string. We fall back to the default/old
	// behavior.
	case ctx.GlobalIsSet("profile") && ctx.GlobalString("profile") == "":
		return profileFromContext(ctx, false, skipMacaroons)

	// There is a file, but no default profile is specified. The user also
	// didn't specify a profile to use so we fall back to the default/old
	// behavior.
	case !ctx.GlobalIsSet("profile") && len(f.Default) == 0:
		return profileFromContext(ctx, false, skipMacaroons)

	// The user didn't specify a profile but there is a default one defined.
	case !ctx.GlobalIsSet("profile") && len(f.Default) > 0:
		profileName = f.Default

	// The user specified a specific profile to use.
	case ctx.GlobalIsSet("profile"):
		profileName = ctx.GlobalString("profile")
	}

	// If we got to here, we do have a profile file and know the name of the
	// profile to use. Now we just need to make sure it does exist.
	for _, prof := range f.Profiles {
		if prof.Name == profileName {
			return prof, nil
		}
	}

	return nil, fmt.Errorf("profile '%s' not found in file %s", profileName,
		defaultProfileFile)
}

// profileFromContext creates an ephemeral profile entry from the global options
// set in the CLI context.
func profileFromContext(ctx *cli.Context, store, skipMacaroons bool) (
	*profileEntry, error) {

	// Parse the paths of the cert and macaroon. This will validate the
	// chain and network value as well.
	tlsCertPath, macPath, err := extractPathArgs(ctx)
	if err != nil {
		return nil, err
	}

	insecure := ctx.GlobalBool("insecure")

	// Load the certificate file now, if specified. We store it as plain PEM
	// directly.
	var tlsCert []byte
	if tlsCertPath != "" && !insecure {
		var err error
		tlsCert, err = os.ReadFile(tlsCertPath)
		if err != nil {
			return nil, fmt.Errorf("could not load TLS cert "+
				"file: %v", err)
		}
	}

	metadata := make(map[string]string)
	for _, m := range ctx.GlobalStringSlice("metadata") {
		pair := strings.Split(m, ":")
		if len(pair) != 2 {
			return nil, fmt.Errorf("invalid format for metadata " +
				"flag; expected \"key:value\"")
		}

		metadata[pair[0]] = pair[1]
	}

	entry := &profileEntry{
		RPCServer: ctx.GlobalString("rpcserver"),
		LndDir: lncfg.CleanAndExpandPath(
			ctx.GlobalString("lnddir"),
		),
		Network:     ctx.GlobalString("network"),
		NoMacaroons: ctx.GlobalBool("no-macaroons"),
		TLSCert:     string(tlsCert),
		Metadata:    metadata,
		Insecure:    insecure,
	}

	// If we aren't using macaroons in general (flag --no-macaroons) or
	// don't need macaroons for this command (wallet unlocker), we can now
	// return already.
	if skipMacaroons || ctx.GlobalBool("no-macaroons") {
		return entry, nil
	}

	// Now load and possibly encrypt the macaroon file.
	macBytes, err := os.ReadFile(macPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read macaroon path (check "+
			"the network setting!): %v", err)
	}
	mac := &macaroon.Macaroon{}
	if err = mac.UnmarshalBinary(macBytes); err != nil {
		return nil, fmt.Errorf("unable to decode macaroon: %w", err)
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
		return nil, fmt.Errorf("unable to store macaroon: %w", err)
	}

	// We determine the name of the macaroon from the file itself but cut
	// off the ".macaroon" at the end.
	macEntry.Name = path.Base(macPath)
	if path.Ext(macEntry.Name) == "macaroon" {
		macEntry.Name = strings.TrimSuffix(macEntry.Name, ".macaroon")
	}

	// Now that we have the macaroon jar as well, let's return the entry
	// with all the values populated.
	entry.Macaroons = &macaroonJar{
		Default: macEntry.Name,
		Timeout: ctx.GlobalInt64("macaroontimeout"),
		IP:      ctx.GlobalString("macaroonip"),
		Jar:     []*macaroonEntry{macEntry},
	}

	return entry, nil
}

// loadProfileFile tries to load the file specified and JSON deserialize it into
// the profile file struct.
func loadProfileFile(file string) (*profileFile, error) {
	if !lnrpc.FileExists(file) {
		return nil, errNoProfileFile
	}

	content, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("could not load profile file %s: %w",
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
		return fmt.Errorf("could not marshal profile: %w", err)
	}

	return os.WriteFile(file, content, 0644)
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
		return nil, fmt.Errorf("error JSON marshalling profile: %w",
			err)
	}

	var out bytes.Buffer
	err = json.Indent(&out, b, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error indenting profile JSON: %w", err)
	}
	out.WriteString("\n")
	return out.Bytes(), nil
}
