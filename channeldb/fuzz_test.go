package channeldb

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/lightningnetwork/lnd/lnwire"
	"github.com/stretchr/testify/require"
)

func getBool(b byte) bool {
	return b%2 == 1
}

// getUint16 requires len(data) >= 2.
func getUint16(data []byte) uint16 {
	return binary.BigEndian.Uint16(data[0:2])
}

// getUint32 requires len(data) >= 4.
func getUint32(data []byte) uint32 {
	return binary.BigEndian.Uint32(data[0:4])
}

// getUint64 requires len(data) >= 8.
func getUint64(data []byte) uint64 {
	return binary.BigEndian.Uint64(data[0:8])
}

// getMilliSatoshi requires len(data) >= 8.
func getMilliSatoshi(data []byte) lnwire.MilliSatoshi {
	return lnwire.MilliSatoshi(getUint64(data[0:8]))
}

// getAmount requires len(data) >= 8.
func getAmount(data []byte) btcutil.Amount {
	return btcutil.Amount(getUint64(data[0:8]))
}

// getBalance requires len(data) >= 9. It returns an *lnwire.MilliSatoshi, with
// a small chance of returning nil.
func getBalance(data []byte) *lnwire.MilliSatoshi {
	if data[0] == 0xff {
		return nil
	}
	msat := getMilliSatoshi(data[1:9])

	return &msat
}

// getHTLCEntries returns the unused data slice and a list of HTLC entries.
func getHTLCEntries(data []byte) ([]byte, []*HTLCEntry) {
	var entries []*HTLCEntry
	for len(data) >= 48 {
		if !getBool(data[0]) {
			data = data[1:]
			break
		}

		entry := &HTLCEntry{
			RefundTimeout: getUint32(data[1:5]),
			OutputIndex:   getUint16(data[5:7]),
			Incoming:      getBool(data[7]),
			Amt:           getAmount(data[8:16]),
		}
		copy(entry.RHash[:], data[16:48])
		entries = append(entries, entry)

		data = data[48:]
	}

	return data, entries
}

func getRevocationLog(data []byte) *RevocationLog {
	if len(data) < 54 {
		return nil
	}

	rl := &RevocationLog{
		OurOutputIndex:   getUint16(data[0:2]),
		TheirOutputIndex: getUint16(data[2:4]),
		OurBalance:       getBalance(data[4:13]),
		TheirBalance:     getBalance(data[13:22]),
	}
	copy(rl.CommitTxHash[:], data[22:54])
	_, rl.HTLCEntries = getHTLCEntries(data[54:])

	return rl
}

// FuzzRevocationLog tests that encoding/decoding RevocationLogs does not modify
// their contents.
func FuzzRevocationLog(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		rl := getRevocationLog(data)
		if rl == nil {
			return
		}

		var b bytes.Buffer
		err := serializeRevocationLog(&b, rl)
		require.NoError(t, err)

		rl2, err := deserializeRevocationLog(&b)
		require.NoError(t, err)

		require.Equal(t, rl, &rl2)
	})
}
