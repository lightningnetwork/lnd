package chanbackup

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/channeldb"
	"github.com/stretchr/testify/require"
)

type mockChannelSource struct {
	chans map[wire.OutPoint]*channeldb.OpenChannel

	failQuery bool

	addrs map[[33]byte][]net.Addr
}

func newMockChannelSource() *mockChannelSource {
	return &mockChannelSource{
		chans: make(map[wire.OutPoint]*channeldb.OpenChannel),
		addrs: make(map[[33]byte][]net.Addr),
	}
}

func (m *mockChannelSource) FetchAllChannels() ([]*channeldb.OpenChannel, error) {
	if m.failQuery {
		return nil, fmt.Errorf("fail")
	}

	chans := make([]*channeldb.OpenChannel, 0, len(m.chans))
	for _, channel := range m.chans {
		chans = append(chans, channel)
	}

	return chans, nil
}

func (m *mockChannelSource) FetchChannel(chanPoint wire.OutPoint) (
	*channeldb.OpenChannel, error) {

	if m.failQuery {
		return nil, fmt.Errorf("fail")
	}

	channel, ok := m.chans[chanPoint]
	if !ok {
		return nil, fmt.Errorf("can't find chan")
	}

	return channel, nil
}

func (m *mockChannelSource) addAddrsForNode(nodePub *btcec.PublicKey, addrs []net.Addr) {
	var nodeKey [33]byte
	copy(nodeKey[:], nodePub.SerializeCompressed())

	m.addrs[nodeKey] = addrs
}

func (m *mockChannelSource) AddrsForNode(_ context.Context,
	nodePub *btcec.PublicKey) (bool, []net.Addr, error) {

	if m.failQuery {
		return false, nil, fmt.Errorf("fail")
	}

	var nodeKey [33]byte
	copy(nodeKey[:], nodePub.SerializeCompressed())

	addrs, ok := m.addrs[nodeKey]

	return ok, addrs, nil
}

// TestFetchBackupForChan tests that we're able to construct a single channel
// backup for channels that are known, unknown, and also channels in which we
// can find addresses for and otherwise.
func TestFetchBackupForChan(t *testing.T) {
	t.Parallel()

	// First, we'll make two channels, only one of them will have all the
	// information we need to construct set of backups for them.
	randomChan1, err := genRandomOpenChannelShell()
	require.NoError(t, err, "unable to generate chan")
	randomChan2, err := genRandomOpenChannelShell()
	require.NoError(t, err, "unable to generate chan")

	chanSource := newMockChannelSource()
	chanSource.chans[randomChan1.FundingOutpoint] = randomChan1
	chanSource.chans[randomChan2.FundingOutpoint] = randomChan2

	chanSource.addAddrsForNode(randomChan1.IdentityPub, []net.Addr{addr1})

	testCases := []struct {
		chanPoint wire.OutPoint

		pass bool
	}{
		// Able to find channel, and addresses, should pass.
		{
			chanPoint: randomChan1.FundingOutpoint,
			pass:      true,
		},

		// Able to find channel, not able to find addrs, should fail.
		{
			chanPoint: randomChan2.FundingOutpoint,
			pass:      false,
		},

		// Not able to find channel, should fail.
		{
			chanPoint: op,
			pass:      false,
		},
	}
	for i, testCase := range testCases {
		_, err := FetchBackupForChan(
			t.Context(), testCase.chanPoint, chanSource, chanSource,
		)
		switch {
		// If this is a valid test case, and we failed, then we'll
		// return an error.
		case err != nil && testCase.pass:
			t.Fatalf("#%v, unable to make chan  backup: %v", i, err)

		// If this is an invalid test case, and we passed it, then
		// we'll return an error.
		case err == nil && !testCase.pass:
			t.Fatalf("#%v got nil error for invalid req: %v",
				i, err)
		}
	}
}

// TestFetchStaticChanBackups tests that we're able to properly query the
// channel source for all channels and construct a Single for each channel.
func TestFetchStaticChanBackups(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	// First, we'll make the set of channels that we want to seed the
	// channel source with. Both channels will be fully populated in the
	// channel source.
	const numChans = 2
	randomChan1, err := genRandomOpenChannelShell()
	require.NoError(t, err, "unable to generate chan")
	randomChan2, err := genRandomOpenChannelShell()
	require.NoError(t, err, "unable to generate chan")

	chanSource := newMockChannelSource()
	chanSource.chans[randomChan1.FundingOutpoint] = randomChan1
	chanSource.chans[randomChan2.FundingOutpoint] = randomChan2
	chanSource.addAddrsForNode(randomChan1.IdentityPub, []net.Addr{addr1})
	chanSource.addAddrsForNode(randomChan2.IdentityPub, []net.Addr{addr2})
	chanSource.addAddrsForNode(randomChan2.IdentityPub, []net.Addr{addr3})
	chanSource.addAddrsForNode(randomChan2.IdentityPub, []net.Addr{addr4})
	chanSource.addAddrsForNode(randomChan2.IdentityPub, []net.Addr{addr5})

	// With the channel source populated, we'll now attempt to create a set
	// of backups for all the channels. This should succeed, as all items
	// are populated within the channel source.
	backups, err := FetchStaticChanBackups(ctx, chanSource, chanSource)
	require.NoError(t, err, "unable to create chan back ups")

	if len(backups) != numChans {
		t.Fatalf("expected %v chans, instead got %v", numChans,
			len(backups))
	}

	// We'll attempt to create a set up backups again, but this time the
	// second channel will have missing information, which should cause the
	// query to fail.
	var n [33]byte
	copy(n[:], randomChan2.IdentityPub.SerializeCompressed())
	delete(chanSource.addrs, n)

	_, err = FetchStaticChanBackups(ctx, chanSource, chanSource)
	if err == nil {
		t.Fatalf("query with incomplete information should fail")
	}

	// To wrap up, we'll ensure that if we're unable to query the channel
	// source at all, then we'll fail as well.
	chanSource = newMockChannelSource()
	chanSource.failQuery = true
	_, err = FetchStaticChanBackups(ctx, chanSource, chanSource)
	if err == nil {
		t.Fatalf("query should fail")
	}
}
