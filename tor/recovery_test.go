package tor

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type controlStep struct {
	command  string
	response string
}

// runControlScript serves a fixed sequence of Tor control commands.
func runControlScript(conn net.Conn, steps []controlStep) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer conn.Close()

		reader := bufio.NewReader(conn)
		for _, step := range steps {
			command, err := reader.ReadString('\n')
			if err != nil {
				result <- err

				return
			}
			if strings.TrimSpace(command) != step.command {
				result <- fmt.Errorf("expected command %q, got %q",
					step.command, strings.TrimSpace(command))

				return
			}

			if _, err := conn.Write([]byte(step.response)); err != nil {
				result <- err

				return
			}
		}

		result <- nil
	}()

	return result
}

type memoryOnionStore struct {
	mu     sync.Mutex
	key    []byte
	writes int
}

func (s *memoryOnionStore) StorePrivateKey(key []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.key = append([]byte(nil), key...)
	s.writes++

	return nil
}

func (s *memoryOnionStore) PrivateKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.key == nil {
		return nil, ErrNoPrivateKey
	}

	return append([]byte(nil), s.key...), nil
}

func (s *memoryOnionStore) DeletePrivateKey() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.key = nil

	return nil
}

func (s *memoryOnionStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.writes
}

func TestRestoreMultipleOnionServices(t *testing.T) {
	client, server := net.Pipe()
	initialResult := runControlScript(server, []controlStep{
		{
			command: "ADD_ONION NEW:ED25519-V3 Port=9735,1001 " +
				"Port=9735,1002",
			response: "250-ServiceID=node-service\r\n" +
				"250-PrivateKey=ED25519-V3:node-key\r\n" +
				"250 OK\r\n",
		},
		{
			command: "ADD_ONION NEW:ED25519-V3 Port=9911,2001",
			response: "250-ServiceID=tower-service\r\n" +
				"250-PrivateKey=ED25519-V3:tower-key\r\n" +
				"250 OK\r\n",
		},
	})

	c := &Controller{
		conn:             textproto.NewConn(client),
		version:          "0.4.8.0",
		started:          1,
		activeServiceIDs: make(map[string]struct{}),
	}
	nodeStore := &memoryOnionStore{}
	towerStore := &memoryOnionStore{}
	nodeCfg := AddOnionConfig{
		VirtualPort: 9735,
		TargetPorts: []int{1001, 1002},
		Store:       nodeStore,
	}
	towerCfg := AddOnionConfig{
		VirtualPort: 9911,
		TargetPorts: []int{2001},
		Store:       towerStore,
	}

	nodeAddr, err := c.AddOnion(nodeCfg)
	require.NoError(t, err)
	towerAddr, err := c.AddOnion(towerCfg)
	require.NoError(t, err)
	require.Equal(t, "node-service.onion:9735", nodeAddr.String())
	require.Equal(t, "tower-service.onion:9911", towerAddr.String())
	require.NoError(t, <-initialResult)
	require.Equal(t, 1, nodeStore.writeCount())
	require.Equal(t, 1, towerStore.writeCount())

	// Mutating the caller's target slice must not change the recorded port
	// mapping used during restoration.
	nodeCfg.TargetPorts[0] = 9999

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	c.controlAddr = listener.Addr().String()
	restoreResult := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			restoreResult <- err

			return
		}

		result := runControlScript(conn, []controlStep{
			{
				command: "PROTOCOLINFO 1",
				response: "250-PROTOCOLINFO 1\r\n" +
					"250-AUTH METHODS=NULL\r\n" +
					"250-VERSION Tor=\"0.4.8.0\"\r\n" +
					"250 OK\r\n",
			},
			{command: "AUTHENTICATE", response: "250 OK\r\n"},
			{
				command: "ADD_ONION ED25519-V3:node-key " +
					"Port=9735,1001 Port=9735,1002",
				response: "250-ServiceID=node-service\r\n" +
					"250 OK\r\n",
			},
			{
				command: "ADD_ONION ED25519-V3:tower-key " +
					"Port=9911,2001",
				response: "250-ServiceID=tower-service\r\n" +
					"250 OK\r\n",
			},
			{
				command: "GETINFO onions/current",
				response: "250-onions/current=tower-service," +
					"node-service\r\n250 OK\r\n",
			},
			{command: "DEL_ONION node-service", response: "250 OK\r\n"},
			{command: "DEL_ONION tower-service", response: "250 OK\r\n"},
		})
		restoreResult <- <-result
	}()
	t.Cleanup(func() {
		_ = listener.Close()
	})

	require.NoError(t, c.Reconnect())
	require.Empty(t, c.activeServiceIDs)
	require.NoError(t, c.RestoreOnionServices())
	require.Len(t, c.activeServiceIDs, 2)
	require.NoError(t, c.CheckOnionService())
	require.NoError(t, c.Stop())
	require.NoError(t, <-restoreResult)
	require.Equal(t, 1, nodeStore.writeCount())
	require.Equal(t, 1, towerStore.writeCount())
}

func TestPartialRestoreCanRetryAfterReconnect(t *testing.T) {
	client, server := net.Pipe()
	partialResult := runControlScript(server, []controlStep{
		{
			command:  "ADD_ONION ED25519-V3:key-one Port=9735,1001",
			response: "250-ServiceID=service-one\r\n250 OK\r\n",
		},
		{
			command:  "ADD_ONION ED25519-V3:key-two Port=9911,2001",
			response: "551 restore failed\r\n",
		},
		{
			command: "GETINFO onions/current",
			response: "250-onions/current=service-one\r\n" +
				"250 OK\r\n",
		},
	})
	c := &Controller{
		conn:    textproto.NewConn(client),
		started: 1,
		registrations: []onionServiceRegistration{
			{
				serviceID: "service-one",
				keyParam:  "ED25519-V3:key-one",
				config: AddOnionConfig{
					VirtualPort: 9735,
					TargetPorts: []int{1001},
				},
			},
			{
				serviceID: "service-two",
				keyParam:  "ED25519-V3:key-two",
				config: AddOnionConfig{
					VirtualPort: 9911,
					TargetPorts: []int{2001},
				},
			},
		},
		activeServiceIDs: make(map[string]struct{}),
	}

	err := c.RestoreOnionServices()
	require.ErrorContains(t, err, "restore onion service service-two")
	require.Contains(t, c.activeServiceIDs, "service-one")
	require.ErrorIs(t, c.CheckOnionService(), ErrServiceIDMismatch)
	require.NoError(t, <-partialResult)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	c.controlAddr = listener.Addr().String()
	retryResult := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			retryResult <- err

			return
		}

		result := runControlScript(conn, []controlStep{
			{
				command: "PROTOCOLINFO 1",
				response: "250-PROTOCOLINFO 1\r\n" +
					"250-AUTH METHODS=NULL\r\n250 OK\r\n",
			},
			{command: "AUTHENTICATE", response: "250 OK\r\n"},
			{
				command: "ADD_ONION ED25519-V3:key-one " +
					"Port=9735,1001",
				response: "250-ServiceID=service-one\r\n250 OK\r\n",
			},
			{
				command: "ADD_ONION ED25519-V3:key-two " +
					"Port=9911,2001",
				response: "250-ServiceID=service-two\r\n250 OK\r\n",
			},
			{command: "DEL_ONION service-one", response: "250 OK\r\n"},
			{command: "DEL_ONION service-two", response: "250 OK\r\n"},
		})
		retryResult <- <-result
	}()
	t.Cleanup(func() {
		_ = listener.Close()
	})

	require.NoError(t, c.Reconnect())
	require.Empty(t, c.activeServiceIDs)
	require.NoError(t, c.RestoreOnionServices())
	require.Len(t, c.activeServiceIDs, 2)
	require.NoError(t, c.Stop())
	require.NoError(t, <-retryResult)
}

func TestRestoreRejectsChangedIdentity(t *testing.T) {
	client, server := net.Pipe()
	result := runControlScript(server, []controlStep{
		{
			command:  "ADD_ONION ED25519-V3:key Port=9735,1001",
			response: "250-ServiceID=changed-service\r\n250 OK\r\n",
		},
		{
			command:  "DEL_ONION changed-service",
			response: "250 OK\r\n",
		},
	})
	c := &Controller{
		conn:    textproto.NewConn(client),
		started: 1,
		registrations: []onionServiceRegistration{{
			serviceID: "original-service",
			keyParam:  "ED25519-V3:key",
			config: AddOnionConfig{
				VirtualPort: 9735,
				TargetPorts: []int{1001},
			},
		}},
		activeServiceIDs: make(map[string]struct{}),
	}

	err := c.RestoreOnionServices()
	require.ErrorIs(t, err, ErrServiceIDMismatch)
	require.Empty(t, c.activeServiceIDs)
	require.NoError(t, <-result)
}

func TestStopDeletesAllServicesAndJoinsErrors(t *testing.T) {
	closeErr := errors.New("close failed")
	conn := &closeErrorConn{
		responses: strings.NewReader(
			"512 Bad arguments\r\n512 Bad arguments\r\n",
		),
		closeErr: closeErr,
	}
	c := &Controller{
		conn: textproto.NewConn(conn),
		registrations: []onionServiceRegistration{
			{serviceID: "service-one"},
			{serviceID: "service-two"},
		},
		activeServiceIDs: map[string]struct{}{
			"service-one": {},
			"service-two": {},
		},
	}

	err := c.Stop()
	require.ErrorIs(t, err, closeErr)
	require.ErrorContains(t, err, "delete onion service service-one")
	require.ErrorContains(t, err, "delete onion service service-two")
	require.Equal(t, "DEL_ONION service-one\r\n"+
		"DEL_ONION service-two\r\n", conn.commands.String())
}
