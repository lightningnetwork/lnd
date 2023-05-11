package wtclient

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// getAddress generates an address from the input data or returns nil if there
// aren't enough bytes remaining in the input data.
func getAddress(data *[]byte) net.Addr {
	if len(*data) < 6 {
		return nil
	}

	addr := &net.TCPAddr{
		IP:   net.IP((*data)[0:4]),
		Port: int(binary.BigEndian.Uint16((*data)[4:6])),
	}

	*data = (*data)[6:]

	return addr
}

// getAddressIterator returns an AddressIterator pre-filled with addresses
// generated from the input data.
func getAddressIterator(data *[]byte) AddressIterator {
	var addrs []net.Addr

	// Always attempt to generate at least one address from the input data.
	// Continue generating addresses if the next data byte is 0xff.
	for addr := getAddress(data); addr != nil; addr = getAddress(data) {
		addrs = append(addrs, addr)

		// Only read another address if the next byte is 0xff.
		if len(*data) < 1 || (*data)[0] != 0xff {
			break
		}
		*data = (*data)[1:]
	}

	iter, err := newAddressIterator(addrs...)
	if err != nil {
		return nil
	}

	return iter
}

// FuzzAddressIterator tests that addressIterator does not panic for any
// sequence of methods called and that the iterator's list is never empty and
// never contains a nil address.
func FuzzAddressIterator(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		iter := getAddressIterator(&data)
		if iter == nil {
			return
		}

		// Use the next byte in data to determine the next method call.
		for len(data) >= 1 {
			cmd := data[0]
			data = data[1:]

			switch cmd {
			case 0x00:
				addr, err := iter.Next()
				if err == nil {
					require.NotNil(t, addr)
				}
			case 0x01:
				addr, err := iter.NextAndLock()
				if err == nil {
					require.NotNil(t, addr)
				}
			case 0x02:
				addr := iter.Peek()
				require.NotNil(t, addr)
			case 0x03:
				addr := iter.PeekAndLock()
				require.NotNil(t, addr)
			case 0x04:
				if addr := getAddress(&data); addr != nil {
					iter.ReleaseLock(addr)
				}
			case 0x05:
				if addr := getAddress(&data); addr != nil {
					iter.Add(addr)
				}
			case 0x06:
				if addr := getAddress(&data); addr != nil {
					_ = iter.Remove(addr)
				}
			case 0x07:
				_ = iter.HasLocked()
			case 0x08:
				addrs := iter.GetAll()
				require.NotEmpty(t, addrs)
				for _, addr := range addrs {
					require.NotNil(t, addr)
				}
			case 0x09:
				iter.Reset()
			case 0x0a:
				iter = iter.Copy()
			default:
				// By returning instead of continuing, we
				// provide backwards compatibility for our
				// corpus. If we continued here, some current
				// inputs would cause a different sequence of
				// events if we later added a new case to the
				// switch.
				return
			}
		}
	})
}
