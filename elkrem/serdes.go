package elkrem

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

//type ElkremNode struct {
//	i   uint64        // index (ith node)
//	h   uint8         // height of this node
//	sha *wire.ShaHash // hash
//}
//type ElkremSender struct {
//	current    uint64        // last sent hash index
//	treeHeight uint8         // height of tree (size is 2**height -1 )
//	maxIndex   uint64        // top of the tree
//	root       *wire.ShaHash // root hash of the tree
//}
//type ElkremReceiver struct {
//	current    uint64       // last received index (actually don't need it?)
//	treeHeight uint8        // height of tree (size is 2**height -1 )
//	s          []ElkremNode // store of received hashes, max size = height
//}

// ToBytes turns an ElkremNode into a 41 byte long byte slice.
// 8 byte index, 1 byte height, 32 byte hash
func (e *ElkremNode) ToBytes() ([]byte, error) {
	var buf bytes.Buffer

	err := binary.Write(&buf, binary.BigEndian, e.i)
	if err != nil {
		return nil, err
	}
	err = binary.Write(&buf, binary.BigEndian, e.h)
	if err != nil {
		return nil, err
	}
	n, err := buf.Write(e.sha.Bytes())
	if err != nil {
		return nil, err
	}
	if n != 32 {
		return nil, fmt.Errorf("%d byte hash, expect 32", n)
	}

	return buf.Bytes(), nil
}

// FromBytes turns an ElkremNode into a 41 byte long byte slice.
// 8 byte index, 1 byte height, 32 byte hash

func ElkremNodeFromBytes([]byte) (ElkremNode, error) {
	var e ElkremNode
	return e, nil
}

func (e *ElkremSender) ToBytes() ([]byte, error) {
	return nil, nil
}

func (e *ElkremSender) FromBytes([]byte) error {
	return nil
}

func (e *ElkremReceiver) ToBytes() ([]byte, error) {
	return nil, nil
}

func (e *ElkremReceiver) FromBytes([]byte) error {
	return nil
}
