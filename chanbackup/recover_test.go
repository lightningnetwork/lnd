package chanbackup

import (
	"bytes"
	"errors"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/lnencrypt"
	"github.com/stretchr/testify/require"
)

var (
	errRestoreFail = errors.New("restore fail")

	errConnectFail = errors.New("connect fail")
)

type mockChannelRestorer struct {
	fail bool

	callCount int
}

func (m *mockChannelRestorer) RestoreChansFromSingles(...Single) error {
	if m.fail {
		return errRestoreFail
	}

	m.callCount++

	return nil
}

type mockPeerConnector struct {
	fail bool

	callCount int
}

func (m *mockPeerConnector) ConnectPeer(_ *btcec.PublicKey,
	_ []net.Addr) error {

	if m.fail {
		return errConnectFail
	}

	m.callCount++

	return nil
}

// TestUnpackAndRecoverSingles tests that we're able to properly unpack and
// recover a set of packed singles.
func TestUnpackAndRecoverSingles(t *testing.T) {
	t.Parallel()

	keyRing := &lnencrypt.MockKeyRing{}

	// First, we'll create a number of single chan backups that we'll
	// shortly back to so we can begin our recovery attempt.
	numSingles := 10
	backups := make([]Single, 0, numSingles)
	var packedBackups PackedSingles
	for i := 0; i < numSingles; i++ {
		channel, err := genRandomOpenChannelShell()
		require.NoError(t, err)

		single := NewSingle(channel, nil)

		var b bytes.Buffer
		err = single.PackToWriter(&b, keyRing)
		require.NoError(t, err)

		backups = append(backups, single)
		packedBackups = append(packedBackups, b.Bytes())
	}

	chanRestorer := mockChannelRestorer{}
	peerConnector := mockPeerConnector{}

	// Now that we have our backups (packed and unpacked), we'll attempt to
	// restore them all in a single batch.

	// If we make the channel restore fail, then the entire method should
	// as well
	chanRestorer.fail = true
	_, err := UnpackAndRecoverSingles(
		packedBackups, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorIs(t, err, errRestoreFail)

	chanRestorer.fail = false

	// If we make the peer connector fail, then the entire method should as
	// well
	peerConnector.fail = true
	_, err = UnpackAndRecoverSingles(
		packedBackups, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorIs(t, err, errConnectFail)

	chanRestorer.callCount--
	peerConnector.fail = false

	// Next, we'll ensure that if all the interfaces function as expected,
	// then the channels will properly be unpacked and restored.
	numRestored, err := UnpackAndRecoverSingles(
		packedBackups, keyRing, &chanRestorer, &peerConnector,
	)
	require.NoError(t, err)
	require.EqualValues(t, numSingles, numRestored)

	// Both the restorer, and connector should have been called 10 times,
	// once for each backup.
	require.EqualValues(
		t, numSingles, chanRestorer.callCount, "restorer call count",
	)
	require.EqualValues(
		t, numSingles, peerConnector.callCount, "peer call count",
	)

	// If we modify the keyRing, then unpacking should fail.
	keyRing.Fail = true
	_, err = UnpackAndRecoverSingles(
		packedBackups, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorContains(t, err, "fail")

	// TODO(roasbeef): verify proper call args
}

// TestUnpackAndRecoverMulti tests that we're able to properly unpack and
// recover a packed multi.
func TestUnpackAndRecoverMulti(t *testing.T) {
	t.Parallel()

	keyRing := &lnencrypt.MockKeyRing{}

	// First, we'll create a number of single chan backups that we'll
	// shortly back to so we can begin our recovery attempt.
	numSingles := 10
	backups := make([]Single, 0, numSingles)
	for i := 0; i < numSingles; i++ {
		channel, err := genRandomOpenChannelShell()
		require.NoError(t, err)

		single := NewSingle(channel, nil)

		backups = append(backups, single)
	}

	multi := Multi{
		StaticBackups: backups,
	}

	var b bytes.Buffer
	err := multi.PackToWriter(&b, keyRing)
	require.NoError(t, err)

	// Next, we'll pack the set of singles into a packed multi, and also
	// create the set of interfaces we need to carry out the remainder of
	// the test.
	packedMulti := PackedMulti(b.Bytes())

	chanRestorer := mockChannelRestorer{}
	peerConnector := mockPeerConnector{}

	// If we make the channel restore fail, then the entire method should
	// as well
	chanRestorer.fail = true
	_, err = UnpackAndRecoverMulti(
		packedMulti, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorIs(t, err, errRestoreFail)

	chanRestorer.fail = false

	// If we make the peer connector fail, then the entire method should as
	// well
	peerConnector.fail = true
	_, err = UnpackAndRecoverMulti(
		packedMulti, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorIs(t, err, errConnectFail)

	chanRestorer.callCount--
	peerConnector.fail = false

	// Next, we'll ensure that if all the interfaces function as expected,
	// then the channels will properly be unpacked and restored.
	numRestored, err := UnpackAndRecoverMulti(
		packedMulti, keyRing, &chanRestorer, &peerConnector,
	)
	require.NoError(t, err)
	require.EqualValues(t, numSingles, numRestored)

	// Both the restorer, and connector should have been called 10 times,
	// once for each backup.
	require.EqualValues(
		t, numSingles, chanRestorer.callCount, "restorer call count",
	)
	require.EqualValues(
		t, numSingles, peerConnector.callCount, "peer call count",
	)

	// If we modify the keyRing, then unpacking should fail.
	keyRing.Fail = true
	_, err = UnpackAndRecoverMulti(
		packedMulti, keyRing, &chanRestorer, &peerConnector,
	)
	require.ErrorContains(t, err, "fail")

	// TODO(roasbeef): verify proper call args
}
