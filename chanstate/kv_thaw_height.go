package chanstate

import (
	"bytes"
	"encoding/binary"

	"github.com/lightningnetwork/lnd/kvdb"
)

var (
	// frozenChanKey is the key where we store the information for any
	// active "frozen" channels. This key is present only in the leaf
	// bucket for a given channel.
	frozenChanKey = []byte("frozen-chans")
)

// FrozenChanKey returns the key used to store a channel's thaw height.
func FrozenChanKey() []byte {
	return frozenChanKey
}

// FetchThawHeight fetches a channel's thaw height from the channel bucket.
func FetchThawHeight(chanBucket kvdb.RBucket) (uint32, error) {
	var height uint32

	heightBytes := chanBucket.Get(frozenChanKey)
	heightReader := bytes.NewReader(heightBytes)

	if err := binary.Read(heightReader, byteOrder, &height); err != nil {
		return 0, err
	}

	return height, nil
}

// StoreThawHeight stores a channel's thaw height in the channel bucket.
func StoreThawHeight(chanBucket kvdb.RwBucket, height uint32) error {
	var heightBuf bytes.Buffer
	if err := binary.Write(&heightBuf, byteOrder, height); err != nil {
		return err
	}

	return chanBucket.Put(frozenChanKey, heightBuf.Bytes())
}

// DeleteThawHeight deletes a channel's thaw height from the channel bucket.
func DeleteThawHeight(chanBucket kvdb.RwBucket) error {
	return chanBucket.Delete(frozenChanKey)
}
