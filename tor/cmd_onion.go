package tor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

var (
	// ErrEncryptedTorPrivateKey is thrown when a tor private key is
	// encrypted, but the user requested an unencrypted key.
	ErrEncryptedTorPrivateKey = errors.New("it appears the Tor private key " +
		"is encrypted but you didn't pass the --tor.encryptkey flag. " +
		"Please restart lnd with the --tor.encryptkey flag or delete " +
		"the Tor key file for regeneration")

	// ErrNoPrivateKey is an error returned by the OnionStore.PrivateKey
	// method when a private key hasn't yet been stored.
	ErrNoPrivateKey = errors.New("private key not found")
)

// OnionType denotes the type of the onion service.
type OnionType int

const (
	// V2 denotes that the onion service is V2.
	V2 OnionType = iota

	// V3 denotes that the onion service is V3.
	V3
)

const (
	// V2KeyParam is a parameter that Tor accepts for a new V2 service.
	V2KeyParam = "RSA1024"

	// V3KeyParam is a parameter that Tor accepts for a new V3 service.
	V3KeyParam = "ED25519-V3"
)

// OnionStore is a store containing information about a particular onion
// service.
type OnionStore interface {
	// StorePrivateKey stores the private key according to the
	// implementation of the OnionStore interface.
	StorePrivateKey([]byte) error

	// PrivateKey retrieves a stored private key. If it is not found, then
	// ErrNoPrivateKey should be returned.
	PrivateKey() ([]byte, error)

	// DeletePrivateKey securely removes the private key from the store.
	DeletePrivateKey() error
}

// EncrypterDecrypter is used for encrypting and decrypting the onion service
// private key.
type EncrypterDecrypter interface {
	EncryptPayloadToWriter([]byte, io.Writer) error
	DecryptPayloadFromReader(io.Reader) ([]byte, error)
}

// OnionFile is a file-based implementation of the OnionStore interface that
// stores an onion service's private key.
type OnionFile struct {
	privateKeyPath string
	privateKeyPerm os.FileMode
	encryptKey     bool
	encrypter      EncrypterDecrypter
}

// A compile-time constraint to ensure OnionFile satisfies the OnionStore
// interface.
var _ OnionStore = (*OnionFile)(nil)

// NewOnionFile creates a file-based implementation of the OnionStore interface
// to store an onion service's private key.
func NewOnionFile(privateKeyPath string, privateKeyPerm os.FileMode,
	encryptKey bool, encrypter EncrypterDecrypter) *OnionFile {

	return &OnionFile{
		privateKeyPath: privateKeyPath,
		privateKeyPerm: privateKeyPerm,
		encryptKey:     encryptKey,
		encrypter:      encrypter,
	}
}

// StorePrivateKey stores the private key at its expected path. It also
// encrypts the key before storing it if requested.
func (f *OnionFile) StorePrivateKey(privateKey []byte) error {
	privateKeyContent := privateKey

	if f.encryptKey {
		var b bytes.Buffer
		err := f.encrypter.EncryptPayloadToWriter(
			privateKey, &b,
		)
		if err != nil {
			return err
		}
		privateKeyContent = b.Bytes()
	}

	err := os.WriteFile(
		f.privateKeyPath, privateKeyContent, f.privateKeyPerm,
	)
	if err != nil {
		return fmt.Errorf("unable to write private key "+
			"to file: %v", err)
	}
	return nil
}

// PrivateKey retrieves the private key from its expected path. If the file does
// not exist, then ErrNoPrivateKey is returned.
func (f *OnionFile) PrivateKey() ([]byte, error) {
	_, err := os.Stat(f.privateKeyPath)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoPrivateKey
	}

	// Try to read the Tor private key to pass into the AddOnion call.
	privateKeyContent, err := os.ReadFile(f.privateKeyPath)
	if err != nil {
		return nil, err
	}

	// If the privateKey starts with either v2 or v3 key params then
	// it's likely not encrypted and we can return the data as is.
	if bytes.HasPrefix(privateKeyContent, []byte(V2KeyParam)) ||
		bytes.HasPrefix(privateKeyContent, []byte(V3KeyParam)) {

		return privateKeyContent, nil
	}

	// If the privateKeyContent is encrypted but --tor.encryptkey
	// wasn't set we return an error.
	if !f.encryptKey {
		return nil, ErrEncryptedTorPrivateKey
	}

	// Attempt to decrypt the key.
	reader := bytes.NewReader(privateKeyContent)
	privateKeyContent, err = f.encrypter.DecryptPayloadFromReader(
		reader,
	)
	if err != nil {
		return nil, err
	}

	return privateKeyContent, nil
}

// DeletePrivateKey removes the file containing the private key.
func (f *OnionFile) DeletePrivateKey() error {
	return os.Remove(f.privateKeyPath)
}

// AddOnionConfig houses all of the required parameters in order to
// successfully create a new onion service or restore an existing one.
type AddOnionConfig struct {
	// Type denotes the type of the onion service that should be created.
	Type OnionType

	// VirtualPort is the externally reachable port of the onion address.
	VirtualPort int

	// TargetPorts is the set of ports that the service will be listening
	// on locally. The Tor server will use choose a random port from this
	// set to forward the traffic from the virtual port.
	//
	// NOTE: If nil/empty, the virtual port will be used as the only target
	// port.
	TargetPorts []int

	// Store is responsible for storing all onion service related
	// information.
	//
	// NOTE: If not specified, then nothing will be stored, making onion
	// services unrecoverable after shutdown.
	Store OnionStore
}

// prepareKeyparam takes a config and prepares the key param to be used inside
// ADD_ONION.
func (c *Controller) prepareKeyparam(cfg AddOnionConfig) (string, error) {
	// We'll start off by checking if the store contains an existing
	// private key. If it does not, then we should request the server to
	// create a new onion service and return its private key. Otherwise,
	// we'll request the server to recreate the onion server from our
	// private key.
	var keyParam string
	switch cfg.Type {
	// TODO(yy): drop support for v2.
	case V2:
		keyParam = "NEW:" + V2KeyParam
	case V3:
		keyParam = "NEW:" + V3KeyParam
	}

	if cfg.Store != nil {
		privateKey, err := cfg.Store.PrivateKey()
		switch err {
		// Proceed to request a new onion service.
		case ErrNoPrivateKey:

		// Recover the onion service with the private key found.
		case nil:
			keyParam = string(privateKey)

		default:
			return "", err
		}
	}

	return keyParam, nil
}

// prepareAddOnion constructs a cmd command string based on the specified
// config.
func (c *Controller) prepareAddOnion(cfg AddOnionConfig) (string, string,
	error) {

	// Create the keyParam.
	keyParam, err := c.prepareKeyparam(cfg)
	if err != nil {
		return "", "", err
	}

	// Now, we'll create a mapping from the virtual port to each target
	// port. If no target ports were specified, we'll use the virtual port
	// to provide a one-to-one mapping.
	var portParam string

	// Helper function which appends the correct Port param depending on
	// whether the user chose to use a custom target IP address or not.
	pushPortParam := func(targetPort int) {
		if c.targetIPAddress == "" {
			portParam += fmt.Sprintf("Port=%d,%d ", cfg.VirtualPort,
				targetPort)
		} else {
			portParam += fmt.Sprintf("Port=%d,%s:%d ",
				cfg.VirtualPort, c.targetIPAddress, targetPort)
		}
	}

	if len(cfg.TargetPorts) == 0 {
		pushPortParam(cfg.VirtualPort)
	} else {
		for _, targetPort := range cfg.TargetPorts {
			pushPortParam(targetPort)
		}
	}

	// Send the command to create the onion service to the Tor server and
	// await its response.
	cmd := fmt.Sprintf("ADD_ONION %s %s", keyParam, portParam)

	return cmd, keyParam, nil
}

// AddOnion creates an ephemeral onion service and returns its onion address.
// Once created, the new onion service will remain active until either,
//   - the onion service is removed via `DEL_ONION`.
//   - the Tor daemon terminates.
//   - the controller connection that originated the `ADD_ONION` is closed.
//
// Each connection can only see its own ephemeral services. If a service needs
// to survive beyond current controller connection, use the "Detach" flag when
// creating new service via `ADD_ONION`.
func (c *Controller) AddOnion(cfg AddOnionConfig) (*OnionAddr, error) {
	// Before sending the request to create an onion service to the Tor
	// server, we'll make sure that it supports V3 onion services if that
	// was the type requested.
	// TODO(yy): drop support for v2.
	if cfg.Type == V3 {
		if err := supportsV3(c.version); err != nil {
			return nil, err
		}
	}

	// Construct the cmd command.
	cmd, keyParam, err := c.prepareAddOnion(cfg)
	if err != nil {
		return nil, err
	}

	// Send the command to create the onion service to the Tor server and
	// await its response.
	_, reply, err := c.sendCommand(cmd)
	if err != nil {
		return nil, err
	}

	// If successful, the reply from the server should be of the following
	// format, depending on whether a private key has been requested:
	//
	//	C: ADD_ONION RSA1024:[Blob Redacted] Port=80,8080
	//	S: 250-ServiceID=testonion1234567
	//	S: 250 OK
	//
	//	C: ADD_ONION NEW:RSA1024 Port=80,8080
	//	S: 250-ServiceID=testonion1234567
	//	S: 250-PrivateKey=RSA1024:[Blob Redacted]
	//	S: 250 OK
	//
	// We're interested in retrieving the service ID, which is the public
	// name of the service, and the private key if requested.
	replyParams := parseTorReply(reply)
	serviceID, ok := replyParams["ServiceID"]
	if !ok {
		return nil, errors.New("service id not found in reply")
	}

	// If a new onion service was created, use the new private key for
	// storage.
	newPrivateKey, ok := replyParams["PrivateKey"]
	if ok {
		keyParam = newPrivateKey
	}

	// If an onion store was provided and a key return wasn't requested,
	// we'll store its private key to disk in the event that it needs to
	// be recreated later on. We write the private key to disk every time
	// in case the user toggles the --tor.encryptkey flag.
	if cfg.Store != nil {
		err := cfg.Store.StorePrivateKey([]byte(keyParam))
		if err != nil {
			return nil, fmt.Errorf("unable to write private key "+
				"to file: %v", err)
		}
	}

	c.activeServiceID = serviceID
	log.Debugf("serviceID:%s added to tor controller", serviceID)

	// Finally, we'll return the onion address composed of the service ID,
	// along with the onion suffix, and the port this onion service can be
	// reached at externally.
	return &OnionAddr{
		OnionService: serviceID + ".onion",
		Port:         cfg.VirtualPort,
		PrivateKey:   keyParam,
	}, nil
}

// DelOnion tells the Tor daemon to remove an onion service, which satisfies
// either,
//   - the onion service was created on the same control connection as the
//     "DEL_ONION" command.
//   - the onion service was created using the "Detach" flag.
func (c *Controller) DelOnion(serviceID string) error {
	log.Debugf("removing serviceID:%s from tor controller", serviceID)

	cmd := fmt.Sprintf("DEL_ONION %s", serviceID)

	// Send the command to create the onion service to the Tor server and
	// await its response.
	code, _, err := c.sendCommand(cmd)

	// Tor replies with "250 OK" on success, or a 512 if there are an
	// invalid number of arguments, or a 552 if it doesn't recognize the
	// ServiceID.
	switch code {
	// Replied 250 OK.
	case success:
		return nil

	// Replied 512 for invalid arguments. This is most likely that the
	// serviceID is not set(empty string).
	case invalidNumOfArguments:
		return fmt.Errorf("invalid arguments: %w", err)

	// Replied 552, which means either,
	//   - the serviceID is invalid.
	//   - the onion service is not owned by the current control connection
	//     and,
	//   - the onion service is not a detached service.
	// In either case, we will ignore the error and log a warning as there
	// not much we can do from the controller side.
	case serviceIDNotRecognized:
		log.Warnf("removing serviceID:%v not found", serviceID)
		return nil

	default:
		return fmt.Errorf("undefined response code: %v, err: %w",
			code, err)
	}
}
