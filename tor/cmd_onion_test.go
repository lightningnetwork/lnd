package tor

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	privateKey = []byte("ED25519-V3 hide_me_plz")
	anotherKey = []byte("ED25519-V3 another_key")
)

// TestOnionFile tests that the File implementation of the OnionStore
// interface behaves as expected.
func TestOnionFile(t *testing.T) {
	t.Parallel()

	tempDir := t.TempDir()
	privateKeyPath := filepath.Join(tempDir, "secret")
	mockEncrypter := MockEncrypter{}

	// Create a new file-based onion store. A private key should not exist
	// yet.
	onionFile := NewOnionFile(
		privateKeyPath, 0600, false, mockEncrypter,
	)
	_, err := onionFile.PrivateKey()
	require.ErrorIs(t, err, ErrNoPrivateKey)

	// Store the private key and ensure what's stored matches.
	err = onionFile.StorePrivateKey(privateKey)
	require.NoError(t, err)

	storePrivateKey, err := onionFile.PrivateKey()
	require.NoError(t, err)
	require.Equal(t, storePrivateKey, privateKey)

	// Finally, delete the private key. We should no longer be able to
	// retrieve it.
	err = onionFile.DeletePrivateKey()
	require.NoError(t, err)

	_, err = onionFile.PrivateKey()
	require.ErrorIs(t, err, ErrNoPrivateKey)

	// Create a new file-based onion store that encrypts the key this time
	// to ensure that an encrypted key is properly handled.
	encryptedOnionFile := NewOnionFile(
		privateKeyPath, 0600, true, mockEncrypter,
	)

	err = encryptedOnionFile.StorePrivateKey(privateKey)
	require.NoError(t, err)

	storedPrivateKey, err := encryptedOnionFile.PrivateKey()
	require.NoError(t, err, "unable to retrieve encrypted private key")
	// Check that PrivateKey returns anotherKey, to make sure the mock
	// decrypter is actually called.
	require.Equal(t, storedPrivateKey, anotherKey)

	err = encryptedOnionFile.DeletePrivateKey()
	require.NoError(t, err)
}

// TestOnionFilePrivateKeyRejectsLegacyV2 ensures the file-backed store
// surfaces ErrNonV3OnionKey when the on-disk key is a plaintext legacy
// v2 (RSA1024) blob, rather than handing the bytes back to Tor.
func TestOnionFilePrivateKeyRejectsLegacyV2(t *testing.T) {
	t.Parallel()

	privateKeyPath := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(
		privateKeyPath,
		[]byte("RSA1024:legacy-v2-key-bytes"),
		0600,
	))

	onionFile := NewOnionFile(privateKeyPath, 0600, false, MockEncrypter{})
	_, err := onionFile.PrivateKey()
	require.ErrorIs(t, err, ErrNonV3OnionKey)
}

// TestPrepareKeyParam checks that the key param is created as expected.
func TestPrepareKeyParam(t *testing.T) {
	v3Key := []byte("ED25519-V3:hide_me_plz")
	dummyErr := errors.New("dummy")

	// Create a dummy controller.
	controller := NewController("", "", "")

	// Test that a V3 keyParam is used.
	cfg := AddOnionConfig{Type: V3}
	keyParam, err := controller.prepareKeyparam(cfg)

	require.Equal(t, "NEW:ED25519-V3", keyParam)
	require.NoError(t, err)

	// Create a mock store which returns a valid v3 private key.
	store := &mockStore{}
	store.On("PrivateKey").Return(v3Key, nil)

	// Check that the stored v3 private key is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Equal(t, string(v3Key), keyParam)
	require.NoError(t, err)
	store.AssertExpectations(t)

	// Create a mock store which returns ErrNoPrivateKey.
	store = &mockStore{}
	store.On("PrivateKey").Return(nil, ErrNoPrivateKey)

	// Check that the V3 keyParam is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Equal(t, "NEW:ED25519-V3", keyParam)
	require.NoError(t, err)
	store.AssertExpectations(t)

	// Create a mock store which returns an dummy error.
	store = &mockStore{}
	store.On("PrivateKey").Return(nil, dummyErr)

	// Check that an error is returned.
	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Empty(t, keyParam)
	require.ErrorIs(t, dummyErr, err)
	store.AssertExpectations(t)

	// A restored legacy v2 (RSA1024) onion key must be rejected; lnd no
	// longer creates or recovers v2 services, and silently regenerating
	// a fresh v3 service would change the advertised onion identity.
	store = &mockStore{}
	store.On("PrivateKey").Return(
		[]byte("RSA1024:legacy-v2-key-bytes"), nil,
	)

	cfg = AddOnionConfig{Type: V3, Store: store}
	keyParam, err = controller.prepareKeyparam(cfg)

	require.Empty(t, keyParam)
	require.ErrorIs(t, err, ErrNonV3OnionKey)
	store.AssertExpectations(t)
}

// TestPrepareAddOnion checks that the cmd used to add onion service is created
// as expected.
func TestPrepareAddOnion(t *testing.T) {
	t.Parallel()

	// Create a mock store.
	store := &mockStore{}
	testKey := []byte("ED25519-V3:hide_me_plz")

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
			expectedCmd:     "ADD_ONION NEW:ED25519-V3 Port=9735,9735 ",
			expectedErr:     nil,
		},
		{
			name:            "specified target IP and empty ports",
			targetIPAddress: "127.0.0.1",
			cfg:             AddOnionConfig{VirtualPort: 9735},
			expectedCmd: "ADD_ONION NEW:ED25519-V3 " +
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
			expectedCmd: "ADD_ONION NEW:ED25519-V3 " +
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
			expectedCmd: "ADD_ONION ED25519-V3:hide_me_plz " +
				"Port=9735,9735 ",
			expectedErr: nil,
		},
	}

	for _, tc := range testCases {
		tc := tc

		if tc.cfg.Store != nil {
			store.On("PrivateKey").Return(
				testKey, tc.expectedErr,
			)
		}

		controller := NewController("", tc.targetIPAddress, "")
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, err := controller.prepareAddOnion(tc.cfg)
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

func (m *mockStore) StorePrivateKey(key []byte) error {
	args := m.Called(key)
	return args.Error(0)
}

func (m *mockStore) PrivateKey() ([]byte, error) {
	args := m.Called()

	// Allow callers to set the returned key bytes via the mock's first
	// return value; fall back to a valid v3 key prefix for tests that
	// only care about a successful key load.
	if key, ok := args.Get(0).([]byte); ok && key != nil {
		return key, args.Error(1)
	}

	return []byte("ED25519-V3:hide_me_plz"), args.Error(1)
}

func (m *mockStore) DeletePrivateKey() error {
	args := m.Called()
	return args.Error(0)
}

type MockEncrypter struct{}

func (m MockEncrypter) EncryptPayloadToWriter(_ []byte, _ io.Writer) error {
	return nil
}

func (m MockEncrypter) DecryptPayloadFromReader(_ io.Reader) ([]byte, error) {
	return anotherKey, nil
}
