package wtclient

import (
	"net"
	"testing"

	"github.com/lightningnetwork/lnd/watchtower/wtdb"
	"github.com/stretchr/testify/require"
)

// TestAddrIterator tests the behaviour of the addressIterator.
func TestAddrIterator(t *testing.T) {
	t.Parallel()

	// Prep some addresses to use in the subtests.
	addr1, err := net.ResolveTCPAddr("tcp", "1.2.3.4:8000")
	require.NoError(t, err)

	addr2, err := net.ResolveTCPAddr("tcp", "1.2.3.4:8001")
	require.NoError(t, err)

	addr3, err := net.ResolveTCPAddr("tcp", "1.2.3.4:8002")
	require.NoError(t, err)

	t.Run("always at least one address", func(t *testing.T) {
		t.Parallel()

		// Assert that an iterator can't be initialised with an empty
		// address list.
		_, err := newAddressIterator()
		require.ErrorContains(t, err, "must have at least one address")

		// Initialise the iterator with addr1.
		iter, err := newAddressIterator(addr1)
		require.NoError(t, err)

		// Attempting to remove addr1 should fail now since it is the
		// only address in the iterator.
		iter.Add(addr1)
		err = iter.Remove(addr1)
		require.ErrorIs(t, err, wtdb.ErrLastTowerAddr)

		// Adding a duplicate of addr1 and then calling Remove should
		// still return an error.
		err = iter.Remove(addr1)
		require.ErrorIs(t, err, wtdb.ErrLastTowerAddr)
	})

	t.Run("address iteration", func(t *testing.T) {
		t.Parallel()

		// Initialise the iterator with addr1.
		iter, err := newAddressIterator(addr1)
		require.NoError(t, err)

		// Add addr2 to the iterator.
		iter.Add(addr2)

		// Check that peek returns addr1.
		a1 := iter.Peek()
		require.NoError(t, err)
		require.Equal(t, addr1, a1)

		// Calling peek multiple times should return the same result.
		a1 = iter.Peek()
		require.Equal(t, addr1, a1)

		// Calling Next should now return addr2.
		a2, err := iter.Next()
		require.NoError(t, err)
		require.Equal(t, addr2, a2)

		// Assert that Peek now returns addr2.
		a2 = iter.Peek()
		require.NoError(t, err)
		require.Equal(t, addr2, a2)

		// Calling Next should result in reaching the end of th list.
		_, err = iter.Next()
		require.ErrorIs(t, err, ErrAddressesExhausted)

		// Calling Peek now should reset the queue and return addr1.
		a1 = iter.Peek()
		require.Equal(t, addr1, a1)

		// Wind the list to the end again so that we can test the Reset
		// func.
		_, err = iter.Next()
		require.NoError(t, err)

		_, err = iter.Next()
		require.ErrorIs(t, err, ErrAddressesExhausted)

		iter.Reset()

		// Now Next should return addr 2.
		a2, err = iter.Next()
		require.NoError(t, err)
		require.Equal(t, addr2, a2)

		// Add addr3 now to ensure that the iteration works even if we
		// are midway through the queue.
		iter.Add(addr3)

		// Now Next should return addr 3.
		a3, err := iter.Next()
		require.NoError(t, err)
		require.Equal(t, addr3, a3)

		// Quickly test that GetAll correctly returns a copy of all the
		// addresses in the iterator.
		addrList := iter.GetAll()
		require.ElementsMatch(
			t, addrList, []net.Addr{addr1, addr2, addr3},
		)

		// Also check that an iterator constructed via the Copy method,
		// also contains all the expected addresses.
		newIterAddrs := iter.Copy().GetAll()
		require.ElementsMatch(
			t, newIterAddrs, []net.Addr{addr1, addr2, addr3},
		)

		// Let's now remove addr3.
		err = iter.Remove(addr3)
		require.NoError(t, err)

		// Since addr3 is gone, Peek should return addr1.
		a1 = iter.Peek()
		require.Equal(t, addr1, a1)
	})

	t.Run("address locking", func(t *testing.T) {
		t.Parallel()

		// Initialise the iterator with addr1 and addr2.
		iter, err := newAddressIterator(addr1, addr2)
		require.NoError(t, err)

		// First we test the locking of an address via the PeekAndLock
		// function.
		a1 := iter.PeekAndLock()
		require.Equal(t, addr1, a1)
		require.True(t, iter.HasLocked())

		// Assert that we can't remove addr1 if there is a lock on it.
		err = iter.Remove(addr1)
		require.ErrorIs(t, err, ErrAddrInUse)

		// Now release the lock on addr1.
		iter.ReleaseLock(addr1)
		require.False(t, iter.HasLocked())

		// Since the lock has been released, we should now be able to
		// remove addr1.
		err = iter.Remove(addr1)
		require.NoError(t, err)

		// Now we test the locking of an address via the NextAndLock
		// function. To do this, we first re-add addr3.
		iter.Add(addr3)

		a2, err := iter.NextAndLock()
		require.NoError(t, err)
		require.Equal(t, addr2, a2)
		require.True(t, iter.HasLocked())

		// Assert that we can't remove addr2 if there is a lock on it.
		err = iter.Remove(addr2)
		require.ErrorIs(t, err, ErrAddrInUse)

		// Now release the lock on addr2.
		iter.ReleaseLock(addr2)
		require.False(t, iter.HasLocked())

		// Since the lock has been released, we should now be able to
		// remove addr1.
		err = iter.Remove(addr2)
		require.NoError(t, err)

		// Only addr3 should still be left in the iterator.
		addrList := iter.GetAll()
		require.Len(t, addrList, 1)
		require.Contains(t, addrList, addr3)

		// Ensure that HasLocked acts correctly in the case where more
		// than one address is being locked and unlock as well as the
		// case where the same address is locked more than once.
		require.False(t, iter.HasLocked())

		a3 := iter.PeekAndLock()
		require.Equal(t, addr3, a3)
		require.True(t, iter.HasLocked())

		a3 = iter.PeekAndLock()
		require.Equal(t, addr3, a3)
		require.True(t, iter.HasLocked())

		iter.Add(addr2)
		a2, err = iter.NextAndLock()
		require.NoError(t, err)
		require.Equal(t, addr2, a2)
		require.True(t, iter.HasLocked())

		// Now release addr2 and assert that HasLock is still true.
		iter.ReleaseLock(addr2)
		require.True(t, iter.HasLocked())

		// Releasing one of the locks on addr3 now should still result
		// in HasLocked returning true.
		iter.ReleaseLock(addr3)
		require.True(t, iter.HasLocked())

		// Releasing it again should now result in should still result
		// in HasLocked returning false.
		iter.ReleaseLock(addr3)
		require.False(t, iter.HasLocked())
	})

	t.Run("calling Next twice without Reset is safe", func(t *testing.T) {
		t.Parallel()

		// Initialise the iterator with addr1.
		iter, err := newAddressIterator(addr1)
		require.NoError(t, err)

		a1 := iter.Peek()
		require.Equal(t, addr1, a1)

		_, err = iter.Next()
		require.ErrorIs(t, err, ErrAddressesExhausted)

		_, err = iter.Next()
		require.ErrorIs(t, err, ErrAddressesExhausted)
	})
}
