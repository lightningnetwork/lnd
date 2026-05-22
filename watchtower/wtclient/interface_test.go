package wtclient

import (
	"net"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/lightningnetwork/lnd/tor"
	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/stretchr/testify/require"
)

// TestNewTowerFromDBTowerFiltersV2Onion asserts that NewTowerFromDBTower drops
// any persisted Tor v2 .onion entries before constructing the address
// iterator, that mixed lists still surface the remaining v3/tcp addresses, and
// that a tower whose addresses are exclusively v2 surfaces
// ErrTowerOnlyV2Onion so the caller can skip it without touching the DB.
func TestNewTowerFromDBTowerFiltersV2Onion(t *testing.T) {
	t.Parallel()

	priv, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	v2 := &tor.OnionAddr{
		OnionService: "3g2upl4pq6kufc4m.onion",
		Port:         9911,
	}
	v3 := &tor.OnionAddr{
		OnionService: "4acth47i6kxnvkewtm6q7ib2s3ufpo5sqbsnz" +
			"jpbi7utijcltosqemad.onion",
		Port: 9911,
	}
	tcp := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9911}

	t.Run("mixed addresses keep v3/tcp", func(t *testing.T) {
		t.Parallel()

		tower, err := NewTowerFromDBTower(&wtdb.Tower{
			ID:          7,
			IdentityKey: priv.PubKey(),
			Addresses:   []net.Addr{v2, v3, tcp, v2},
		})
		require.NoError(t, err)
		require.Equal(t, []net.Addr{v3, tcp}, tower.Addresses.GetAll())
	})

	t.Run("only v2 addresses returns sentinel", func(t *testing.T) {
		t.Parallel()

		_, err := NewTowerFromDBTower(&wtdb.Tower{
			ID:          7,
			IdentityKey: priv.PubKey(),
			Addresses:   []net.Addr{v2, v2},
		})
		require.ErrorIs(t, err, ErrTowerOnlyV2Onion)
	})
}
