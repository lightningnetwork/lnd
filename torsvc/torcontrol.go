package torsvc

import (
	"bufio"
	"fmt"
	"net"
	"net/textproto"
	"strings"
)

const (
	addStr  = "ADD_ONION"
	authStr = "AUTHENTICATE"
	delStr  = "DEL_ONION"
	success = 250
)

// TorControl houses options for interacting with Tor's ControlPort. These
// options determine the hidden service creation configuration that LND will use
// when automatically creating hidden services.
type TorControl struct {
	conn     net.Conn
	reader   *textproto.Reader
	Password string
	Port     string
	TargPort string
	VirtPort string
	PrivKey  string
	Save     bool
}

// AuthWithPass authenticates via password to Tor's ControlPort.
//
// Note: AuthWithPass must be called even if the ControlPort has no
// authentication mechanism in place.
func (tc *TorControl) AuthWithPass() error {
	_, _, err := tc.sendCommand(authStr + " \"" + tc.Password + "\"\n")
	return err
}

// AddOnion creates a Tor v2 hidden service. This hidden service is available as
// long as the connection to Tor's ControlPort is kept open.
func (tc *TorControl) AddOnion() (string, error) {
	var command string

	// If a private key is provided to the AddOnion command, we use it instead
	// of creating a new v2 hidden service.
	if tc.PrivKey != "" {
		command = addStr + " RSA1024:" + tc.PrivKey
	} else {
		command = addStr + " NEW:RSA1024"
	}

	// If we are not going to save this private key, set the DiscardPk flag.
	if !tc.Save {
		command += " Flags=DiscardPk"
	}

	// Add the VIRTPORT and TARGET ports to the command.
	command += " Port=" + tc.VirtPort + "," + tc.TargPort + "\n"

	// Send the command to Tor's ControlPort.
	_, message, err := tc.sendCommand(command)
	if tc.Save {
		// TODO(eugene)
		// Parse out the private key from response and store it somewhere.
	}

	// Parse out the hidden service from the response and return it.
	i := strings.Index(message, "ServiceID")
	if i == -1 {
		return "", fmt.Errorf("Could not retrieve hidden service")
	}

	messageStr := message[i:]

	j := strings.Index(messageStr, "=")
	if j == -1 {
		return "", fmt.Errorf("Could not retrieve hidden service")
	}

	k := strings.Index(messageStr, "\n")
	if k == -1 {
		return "", fmt.Errorf("Could not retrieve hidden service")
	}

	if j+1 >= k {
		return "", fmt.Errorf("Could not retrieve hidden service")
	}

	return messageStr[j+1 : k], err
}

// DelOnion deletes a Tor v2 hidden service given its ServiceID.
func (tc *TorControl) DelOnion() error {
	// TODO(eugene)
	return nil
}

// Open opens the connection to Tor's ControlPort.
func (tc *TorControl) Open() error {
	var err error
	tc.conn, err = net.Dial("tcp", "localhost:"+tc.Port)
	if err != nil {
		return err
	}

	reader := bufio.NewReader(tc.conn)
	tc.reader = textproto.NewReader(reader)

	return nil
}

// Close closes the connection to Tor's ControlPort.
func (tc *TorControl) Close() error {
	if err := tc.conn.Close(); err != nil {
		return err
	}
	tc.reader = nil
	return nil
}

// sendCommand sends a command for execution to Tor's ControlPort.
func (tc *TorControl) sendCommand(command string) (int, string, error) {
	// Write command to Tor's ControlPort.
	_, err := tc.conn.Write([]byte(command))
	if err != nil {
		return 0, "", fmt.Errorf("Writing to Tor's ControlPort failed: %s", err)
	}

	// ReadResponse supports multi-line responses whereas ReadCodeLine does not.
	// In some cases, the response will need to be parsed out from the message
	// variable.
	code, message, err := tc.reader.ReadResponse(success)
	if err != nil {
		return code, message, fmt.Errorf("Reading Tor's ControlPort "+
			"command status failed: %s", err)
	}

	return code, message, nil
}
