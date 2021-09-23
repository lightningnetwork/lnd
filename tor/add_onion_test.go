package tor

import (
	"bytes"
	"errors"
	"io/ioutil"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// TestOnionFile tests that the OnionFile implementation of the OnionStore
// interface behaves as expected.
func TestOnionFile(t *testing.T) {
	t.Parallel()

	tempDir, err := ioutil.TempDir("", "onion_store")
	if err != nil {
		t.Fatalf("unable to create temp dir: %v", err)
	}

	privateKey := []byte("hide_me_plz")
	privateKeyPath := filepath.Join(tempDir, "secret")

	// Create a new file-based onion store. A private key should not exist
	// yet.
	onionFile := NewOnionFile(privateKeyPath, 0600)
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatalf("expected ErrNoPrivateKey, got \"%v\"", err)
	}

	// Store the private key and ensure what's stored matches.
	if err := onionFile.StorePrivateKey(V2, privateKey); err != nil {
		t.Fatalf("unable to store private key: %v", err)
	}
	storePrivateKey, err := onionFile.PrivateKey(V2)
	if err != nil {
		t.Fatalf("unable to retrieve private key: %v", err)
	}
	if !bytes.Equal(storePrivateKey, privateKey) {
		t.Fatalf("expected private key \"%v\", got \"%v\"",
			string(privateKey), string(storePrivateKey))
	}

	// Finally, delete the private key. We should no longer be able to
	// retrieve it.
	if err := onionFile.DeletePrivateKey(V2); err != nil {
		t.Fatalf("unable to delete private key: %v", err)
	}
	if _, err := onionFile.PrivateKey(V2); err != ErrNoPrivateKey {
		t.Fatal("found deleted private key")
	}
}

// TestPrepareKeyParam checks that the key param is created as expected.
func TestPrepareKeyParam(t *testing.T) {
	testKey := []byte("hide_me_plz")
	dummyErr := errors.New("dummy")

	// Create a dummy controller.
	controller := NewController("", "", "")

	// Test that a V3 keyParam is used.
	cfg := AddOnionConfig{Type: V3}
	keyParam, err := controller.prepareKeyparam(cfg)

	require.Equal(t, "NEW:ED25519-V3", keyParam)
	require.NoError(t, err)

	// Create a mock store which returns the test private key.
	store := &mockStore{}
	store.On("PrivateKey", cfg.Type).Return(testKey, nil)

	// Check that the test private is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Equal(t, string(testKey), keyParam)
	require.NoError(t, err)
	store.AssertExpectations(t)

	// Create a mock store which returns ErrNoPrivateKey.
	store = &mockStore{}
	store.On("PrivateKey", cfg.Type).Return(nil, ErrNoPrivateKey)

	// Check that the V3 keyParam is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Equal(t, "NEW:ED25519-V3", keyParam)
	require.NoError(t, err)
	store.AssertExpectations(t)

	// Create a mock store which returns an dummy error.
	store = &mockStore{}
	store.On("PrivateKey", cfg.Type).Return(nil, dummyErr)

	// Check that an error is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Empty(t, keyParam)
	require.ErrorIs(t, dummyErr, err)
	store.AssertExpectations(t)
}

// TestPrepareAddOnion checks that the cmd used to add onion service is created
// as expected.
func TestPrepareAddOnion(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &mockStore{}
	testKey := []byte("hide_me_plz")

	testCases := []struct {
		name            string
		targetIPAddress string
		cfg             AddOnionConfig
		expectedCmd     string
		expectedErr     error
	}{
		{
			name:            "empty target IP and ports",
			targetIPAddress: "",
			cfg:             AddOnionConfig{VirtualPort: 9735},
			expectedCmd:     "ADD_ONION NEW:RSA1024 Port=9735,9735 ",
			expectedErr:     nil,
		},
		{
			name:            "specified target IP and empty ports",
			targetIPAddress: "127.0.0.1",
			cfg:             AddOnionConfig{VirtualPort: 9735},
			expectedCmd: "ADD_ONION NEW:RSA1024 " +
				"Port=9735,127.0.0.1:9735 ",
			expectedErr: nil,
		},
		{
			name:            "specified target IP and ports",
			targetIPAddress: "127.0.0.1",
			cfg: AddOnionConfig{
				VirtualPort: 9735,
				TargetPorts: []int{18000, 18001},
			},
			expectedCmd: "ADD_ONION NEW:RSA1024 " +
				"Port=9735,127.0.0.1:18000 " +
				"Port=9735,127.0.0.1:18001 ",
			expectedErr: nil,
		},
		{
			name:            "specified private key from store",
			targetIPAddress: "",
			cfg: AddOnionConfig{
				VirtualPort: 9735,
				Store:       store,
			},
			expectedCmd: "ADD_ONION hide_me_plz " +
				"Port=9735,9735 ",
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		if tc.cfg.Store != nil {
			store.On("PrivateKey", tc.cfg.Type).Return(
				testKey, tc.expectedErr,
			)
		}

		controller := NewController("", tc.targetIPAddress, "")
		t.Run(tc.name, func(t *testing.T) {
			cmd, err := controller.prepareAddOnion(tc.cfg)
			require.Equal(t, tc.expectedErr, err)
			require.Equal(t, tc.expectedCmd, cmd)

			// Check that the mocker is satisfied.
			store.AssertExpectations(t)
		})
	}

}

// mockStore implements a mock of the interface OnionStore.
type mockStore struct {
	mock.Mock
}

// A compile-time constraint to ensure mockStore satisfies the OnionStore
// interface.
var _ OnionStore = (*mockStore)(nil)

func (m *mockStore) StorePrivateKey(ot OnionType, key []byte) error {
	args := m.Called(ot, key)
	return args.Error(0)
}

func (m *mockStore) PrivateKey(ot OnionType) ([]byte, error) {
	args := m.Called(ot)
	if args.Get(0) == nil {
		return nil, args.Error(1)
	}
	return args.Get(0).([]byte), args.Error(1)
}

func (m *mockStore) DeletePrivateKey(ot OnionType) error {
	args := m.Called(ot)
	return args.Error(0)
}
